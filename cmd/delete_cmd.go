package cmd

import (
	"errors"
	"fmt"
	"time"

	bosherr "github.com/cloudfoundry/bosh-agent/errors"
	boshlog "github.com/cloudfoundry/bosh-agent/logger"
	boshsys "github.com/cloudfoundry/bosh-agent/system"

	bmconfig "github.com/cloudfoundry/bosh-micro-cli/config"
	bmcpi "github.com/cloudfoundry/bosh-micro-cli/cpi"
	bmagentclient "github.com/cloudfoundry/bosh-micro-cli/deployer/agentclient"
	bmdisk "github.com/cloudfoundry/bosh-micro-cli/deployer/disk"
	bmstemcell "github.com/cloudfoundry/bosh-micro-cli/deployer/stemcell"
	bmdepl "github.com/cloudfoundry/bosh-micro-cli/deployment"
	bmeventlog "github.com/cloudfoundry/bosh-micro-cli/eventlogger"
	bmrel "github.com/cloudfoundry/bosh-micro-cli/release"
	bmui "github.com/cloudfoundry/bosh-micro-cli/ui"

	bminstance "github.com/cloudfoundry/bosh-micro-cli/deployer/instance"
	bmvm "github.com/cloudfoundry/bosh-micro-cli/deployer/vm"
)

type deleteCmd struct {
	ui                     bmui.UI
	userConfig             bmconfig.UserConfig
	fs                     boshsys.FileSystem
	deploymentParser       bmdepl.Parser
	cpiInstaller           bmcpi.Installer
	vmManagerFactory       bmvm.ManagerFactory
	instanceManagerFactory bminstance.ManagerFactory
	vmRepo                 bmconfig.VMRepo
	diskRepo               bmconfig.DiskRepo
	stemcellRepo           bmconfig.StemcellRepo
	agentClientFactory     bmagentclient.Factory
	eventLogger            bmeventlog.EventLogger
	logger                 boshlog.Logger
	logTag                 string
}

func NewDeleteCmd(ui bmui.UI,
	userConfig bmconfig.UserConfig,
	fs boshsys.FileSystem,
	deploymentParser bmdepl.Parser,
	cpiInstaller bmcpi.Installer,
	vmManagerFactory bmvm.ManagerFactory,
	instanceManagerFactory bminstance.ManagerFactory,
	vmRepo bmconfig.VMRepo,
	diskRepo bmconfig.DiskRepo,
	stemcellRepo bmconfig.StemcellRepo,
	agentClientFactory bmagentclient.Factory,
	eventLogger bmeventlog.EventLogger,
	logger boshlog.Logger) *deleteCmd {
	return &deleteCmd{
		ui:                     ui,
		userConfig:             userConfig,
		fs:                     fs,
		deploymentParser:       deploymentParser,
		cpiInstaller:           cpiInstaller,
		vmManagerFactory:       vmManagerFactory,
		instanceManagerFactory: instanceManagerFactory,
		vmRepo:                 vmRepo,
		diskRepo:               diskRepo,
		stemcellRepo:           stemcellRepo,
		agentClientFactory:     agentClientFactory,
		eventLogger:            eventLogger,
		logger:                 logger,
		logTag:                 "deleteCmd",
	}
}

func (c *deleteCmd) Name() string {
	return "delete"
}

func (c *deleteCmd) Run(args []string) error {
	cpiReleaseTarballPath, err := c.parseCmdInputs(args)
	if err != nil {
		return err
	}

	cpiDeployment, cpiRelease, err := c.validateInputFiles(cpiReleaseTarballPath)
	if err != nil {
		return err
	}
	defer cpiRelease.Delete()

	cloud, err := c.cpiInstaller.Install(cpiDeployment, cpiRelease)
	if err != nil {
		return bosherr.WrapError(err, "Installing CPI deployment")
	}

	deleteStage := c.eventLogger.NewStage("deleting deployment")
	deleteStage.Start()

	vmManager := c.vmManagerFactory.NewManager(cloud, cpiDeployment.Mbus)
	instanceManager := c.instanceManagerFactory.NewManager(cloud, vmManager)

	instances, err := instanceManager.FindCurrent()
	if err != nil {
		return bosherr.WrapError(err, "Finding current deployment instances")
	}

	diskManager := bmdisk.NewManagerFactory(c.diskRepo, c.logger).NewManager(cloud)
	disk, diskFound, err := diskManager.FindCurrent()
	if err != nil {
		return bosherr.WrapError(err, "Finding current deployment disk")
	}

	stemcellManager := bmstemcell.NewManagerFactory(c.stemcellRepo, c.eventLogger).NewManager(cloud)
	stemcell, stemcellFound, err := stemcellManager.FindCurrent()
	if err != nil {
		return bosherr.WrapError(err, "Finding current deployment stemcell")
	}

	pingTimeout := 10 * time.Second
	pingDelay := 500 * time.Millisecond
	for _, instance := range instances {
		if err = instance.Delete(pingTimeout, pingDelay, deleteStage); err != nil {
			return err
		}
	}

	if diskFound {
		if err = c.deleteCurrentDisk(deleteStage, disk); err != nil {
			return err
		}
	}

	if stemcellFound {
		if err = c.deleteCurrentStemcell(deleteStage, stemcell); err != nil {
			return err
		}
	}

	if err = c.deleteOrphanedDisks(deleteStage, diskManager); err != nil {
		return err
	}

	if err = c.deleteOrphanedStemcells(deleteStage, stemcellManager); err != nil {
		return err
	}

	deleteStage.Finish()

	return nil
}

func (c *deleteCmd) validateInputFiles(releaseTarballPath string) (
	cpiDeployment bmdepl.CPIDeployment,
	cpiRelease bmrel.Release,
	err error,
) {
	validationStage := c.eventLogger.NewStage("validating")
	validationStage.Start()

	err = validationStage.PerformStep("Validating deployment manifest", func() error {
		if c.userConfig.DeploymentFile == "" {
			return bosherr.Error("No deployment set")
		}

		deploymentFilePath := c.userConfig.DeploymentFile

		c.logger.Info(c.logTag, "Checking for deployment '%s'", deploymentFilePath)
		if !c.fs.FileExists(deploymentFilePath) {
			return bosherr.Errorf("Verifying that the deployment '%s' exists", deploymentFilePath)
		}

		_, cpiDeployment, err = c.deploymentParser.Parse(deploymentFilePath)
		if err != nil {
			return bosherr.WrapErrorf(err, "Parsing deployment manifest '%s'", deploymentFilePath)
		}

		return nil
	})
	if err != nil {
		return cpiDeployment, nil, err
	}

	err = validationStage.PerformStep("Validating cpi release", func() error {
		if !c.fs.FileExists(releaseTarballPath) {
			return bosherr.Errorf("Verifying that the CPI release '%s' exists", releaseTarballPath)
		}

		cpiRelease, err = c.cpiInstaller.Extract(releaseTarballPath)
		if err != nil {
			return bosherr.WrapErrorf(err, "Extracting CPI release '%s'", releaseTarballPath)
		}

		return nil
	})
	if err != nil {
		return cpiDeployment, cpiRelease, err
	}

	validationStage.Finish()

	return cpiDeployment, cpiRelease, nil
}

func (c *deleteCmd) parseCmdInputs(args []string) (string, error) {
	if len(args) != 1 {
		c.ui.Error("Invalid usage - delete command requires exactly 1 argument")
		c.ui.Sayln("Expected usage: bosh-micro delete <cpi-release-tarball>")
		c.logger.Error(c.logTag, "Invalid arguments: %#v", args)
		return "", errors.New("Invalid usage - delete command requires exactly 1 argument")
	}
	return args[0], nil
}

func (c *deleteCmd) deleteCurrentDisk(deleteStage bmeventlog.Stage, disk bmdisk.Disk) error {
	stepName := fmt.Sprintf("Deleting disk '%s'", disk.CID())
	return deleteStage.PerformStep(stepName, func() error {
		if err := disk.Delete(); err != nil {
			return err
		}
		if err := c.diskRepo.ClearCurrent(); err != nil {
			return bosherr.WrapErrorf(err, "Deleting deployment disk record '%s'", disk.CID())
		}
		return nil
	})
}

func (c *deleteCmd) deleteCurrentStemcell(deleteStage bmeventlog.Stage, stemcell bmstemcell.CloudStemcell) error {
	stepName := fmt.Sprintf("Deleting stemcell '%s'", stemcell.CID())
	return deleteStage.PerformStep(stepName, func() error {
		if err := stemcell.Delete(); err != nil {
			return err
		}
		if err := c.stemcellRepo.ClearCurrent(); err != nil {
			return bosherr.WrapErrorf(err, "Deleting deployment stemcell record '%s'", stemcell.CID())
		}
		return nil
	})
}

func (c *deleteCmd) deleteOrphanedDisks(deleteStage bmeventlog.Stage, diskManager bmdisk.Manager) error {
	unusedDisks, err := diskManager.FindUnused()
	if err != nil {
		return bosherr.WrapErrorf(err, "Finding orphaned disks")
	}
	for _, disk := range unusedDisks {
		stepName := fmt.Sprintf("Deleting orphaned disk '%s'", disk.CID())
		err = deleteStage.PerformStep(stepName, func() error {
			return disk.Delete()
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *deleteCmd) deleteOrphanedStemcells(deleteStage bmeventlog.Stage, stemcellManager bmstemcell.Manager) error {
	unusedStemcells, err := stemcellManager.FindUnused()
	if err != nil {
		return bosherr.WrapErrorf(err, "Finding orphaned stemcells")
	}
	for _, stemcell := range unusedStemcells {
		stepName := fmt.Sprintf("Deleting orphaned stemcell '%s'", stemcell.CID())
		err = deleteStage.PerformStep(stepName, func() error {
			return stemcell.Delete()
		})
		if err != nil {
			return err
		}
	}
	return nil
}