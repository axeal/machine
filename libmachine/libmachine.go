package libmachine

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/rancher/machine/drivers/errdriver"
	"github.com/rancher/machine/libmachine/auth"
	"github.com/rancher/machine/libmachine/cert"
	"github.com/rancher/machine/libmachine/check"
	"github.com/rancher/machine/libmachine/drivers"
	"github.com/rancher/machine/libmachine/drivers/plugin/localbinary"
	rpcdriver "github.com/rancher/machine/libmachine/drivers/rpc"
	"github.com/rancher/machine/libmachine/engine"
	"github.com/rancher/machine/libmachine/host"
	"github.com/rancher/machine/libmachine/log"
	"github.com/rancher/machine/libmachine/mcnerror"
	"github.com/rancher/machine/libmachine/mcnutils"
	"github.com/rancher/machine/libmachine/persist"
	"github.com/rancher/machine/libmachine/provision"
	"github.com/rancher/machine/libmachine/ssh"
	"github.com/rancher/machine/libmachine/state"
	"github.com/rancher/machine/libmachine/swarm"
	"github.com/rancher/machine/libmachine/version"
)

type API interface {
	io.Closer
	NewHost(driverName string, rawDriver []byte) (*host.Host, error)
	Create(h *host.Host) error
	persist.Store
	GetMachinesDir() string
}

type Client struct {
	certsDir       string
	IsDebug        bool
	SSHClientType  ssh.ClientType
	GithubAPIToken string
	persist.Store
	clientDriverFactory rpcdriver.RPCClientDriverFactory
}

func NewClient(storePath, certsDir string) *Client {
	return &Client{
		certsDir:            certsDir,
		IsDebug:             false,
		SSHClientType:       ssh.External,
		Store:               persist.NewFilestore(storePath, certsDir, certsDir),
		clientDriverFactory: rpcdriver.NewRPCClientDriverFactory(),
	}
}

func (api *Client) NewHost(driverName string, rawDriver []byte) (*host.Host, error) {
	driver, err := api.clientDriverFactory.NewRPCClientDriver(driverName, rawDriver)
	if err != nil {
		return nil, err
	}

	return &host.Host{
		ConfigVersion: version.ConfigVersion,
		Name:          driver.GetMachineName(),
		Driver:        driver,
		DriverName:    driver.DriverName(),
		HostOptions: &host.Options{
			AuthOptions: &auth.Options{
				CertDir:          api.certsDir,
				CaCertPath:       filepath.Join(api.certsDir, "ca.pem"),
				CaPrivateKeyPath: filepath.Join(api.certsDir, "ca-key.pem"),
				ClientCertPath:   filepath.Join(api.certsDir, "cert.pem"),
				ClientKeyPath:    filepath.Join(api.certsDir, "key.pem"),
				ServerCertPath:   filepath.Join(api.GetMachinesDir(), "server.pem"),
				ServerKeyPath:    filepath.Join(api.GetMachinesDir(), "server-key.pem"),
			},
			EngineOptions: &engine.Options{
				InstallURL:    drivers.DefaultEngineInstallURL,
				StorageDriver: provision.DefaultStorageDriver,
				TLSVerify:     true,
			},
			SwarmOptions: &swarm.Options{
				Host:     "tcp://0.0.0.0:3376",
				Image:    "swarm:latest",
				Strategy: "spread",
			},
		},
	}, nil
}

func (api *Client) Load(name string) (*host.Host, error) {
	h, err := api.Store.Load(name)
	if err != nil {
		return nil, err
	}

	d, err := api.clientDriverFactory.NewRPCClientDriver(h.DriverName, h.RawDriver)
	if err != nil {
		// Not being able to find a driver binary is a "known error"
		if _, ok := err.(localbinary.ErrPluginBinaryNotFound); ok {
			h.Driver = errdriver.NewDriver(h.DriverName)
			return h, nil
		}
		return nil, err
	}

	if h.DriverName == "virtualbox" {
		h.Driver = drivers.NewSerialDriver(d)
	} else {
		h.Driver = d
	}

	return h, nil
}

// Create is the wrapper method which covers all of the boilerplate around
// actually creating, provisioning, and persisting an instance in the store.
func (api *Client) Create(h *host.Host) error {
	if h.HostOptions.CustomInstallScript == "" {
		if err := cert.BootstrapCertificates(h.AuthOptions()); err != nil {
			return fmt.Errorf("Error generating certificates: %s", err)
		}
	}

	log.Info("Running pre-create checks...")

	if err := h.Driver.PreCreateCheck(); err != nil {
		return mcnerror.ErrDuringPreCreate{
			Cause: err,
		}
	}

	if err := api.Save(h); err != nil {
		return fmt.Errorf("Error saving host to store before attempting creation: %s", err)
	}

	log.Info("Creating machine...")

	if err := api.performCreate(h); err != nil {
		return fmt.Errorf("Error creating machine: %s", err)
	}

	log.Debug("Reticulating splines...")

	return nil
}

func (api *Client) performCreate(h *host.Host) error {
	if err := h.Driver.Create(); err != nil {
		return fmt.Errorf("Error in driver during machine creation: %s", err)
	}

	if err := api.Save(h); err != nil {
		return fmt.Errorf("Error saving host to store after attempting creation: %s", err)
	}

	// TODO: Not really a fan of just checking "none" or "ci-test" here.
	if h.Driver.DriverName() == "none" || h.Driver.DriverName() == "ci-test" {
		return nil
	}

	log.Info("Waiting for machine to be running, this may take a few minutes...")
	if err := mcnutils.WaitFor(drivers.MachineInState(h.Driver, state.Running)); err != nil {
		return fmt.Errorf("Error waiting for machine to be running: %s", err)
	}

	if h.HostOptions.CustomInstallScript != "" && drivers.DriverUserdataFlag(h.Driver) != "" {
		log.Infof("Custom install script was sent via userdata, provisioning complete...")
		return nil
	}

	log.Info("Detecting operating system of created instance...")
	provisioner, err := provision.DetectProvisioner(h.Driver)
	if err != nil {
		return fmt.Errorf("Error detecting OS: %s", err)
	}

	log.Infof("Provisioning with %s...", provisioner.String())
	if h.HostOptions.CustomInstallScript != "" {
		log.Infof("Provisioning with custom install script via SSH, not installing Docker...")
		return provision.WithCustomScript(provisioner, h.HostOptions.CustomInstallScript)
	} else {
		if err := provisioner.Provision(*h.HostOptions.SwarmOptions, *h.HostOptions.AuthOptions, *h.HostOptions.EngineOptions); err != nil {
			return err
		}
	}

	// We should check the connection to docker here
	log.Info("Checking connection to Docker...")
	if _, _, err = check.DefaultConnChecker.Check(h, false); err != nil {
		return fmt.Errorf("Error checking the host: %s", err)
	}

	log.Info("Docker is up and running!")
	return nil
}

func (api *Client) Close() error {
	return api.clientDriverFactory.Close()
}
