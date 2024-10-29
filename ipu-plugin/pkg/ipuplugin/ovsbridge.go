package ipuplugin

import (
	"fmt"
	"os/exec"

	"github.com/intel/ipu-opi-plugins/ipu-plugin/pkg/types"
	"github.com/intel/ipu-opi-plugins/ipu-plugin/pkg/utils"
	log "github.com/sirupsen/logrus"
)

type ovsBridge struct {
	brVfName  string
	ovsCliDir string
}

const (
	brPhy1Name          = "br-phy-1"
	brPhy2Name          = "br-phy-2"
	brPhy1PortInterface = "enp0s1f0d4"
	brPhy2PortInterface = "enp0s1f0d5"
	//IP for port connected to Link partner 1
	ACC_PR_PHY1_IP = "192.168.10.230/24"
	//#IP for port connected to Link partner 2
	ACC_PR_PHY2_IP = "192.168.20.230/24"
	ACC_VM_PR_IP   = "192.168.100.252/24"
)

func NewOvsBridgeController(bridgeName, ovsCliDir string) types.BridgeController {
	return &ovsBridge{
		brVfName:  bridgeName,
		ovsCliDir: ovsCliDir,
	}
}

// TODO: Currently we use ovs-vsctl in this path->b.ovsCliDir+"/ovs-vsctl",
// to check, if this is fine, given that OVS P4 offload is currently disabled and redhat may use vanilla OVS tool.
// Adding the bridges for link-side phy port0 and port1 and port interfaces for F5 use-case.
func LinkSideBridgeAndPortSetupForF5(b *ovsBridge) error {
	linkBridgesSetup := make(map[string]string)

	linkBridgesSetup[brPhy1Name] = brPhy1PortInterface
	linkBridgesSetup[brPhy2Name] = brPhy2PortInterface

	var cmdParams []string
	var ipAddr string
	for brName, brPortInterfaceName := range linkBridgesSetup {
		//TODO: del-br code to be removed later.
		cmdParams = []string{"--may-exist", "del-br", brName}
		if err := utils.ExecOsCommand(b.ovsCliDir+"/ovs-vsctl", cmdParams...); err != nil {
			log.Infof("Can ignore errors->%s, in deleting ovs bridge %s since it may not exist", err.Error(), brName)
		}
		cmdParams = []string{"--may-exist", "add-br", brName}
		if err := utils.ExecOsCommand(b.ovsCliDir+"/ovs-vsctl", cmdParams...); err != nil {
			return fmt.Errorf("error creating ovs bridge %s with ovs-vsctl command %s", brName, err.Error())
		}
		cmdParams = []string{"add-port", brName, brPortInterfaceName}
		if err := utils.ExecOsCommand(b.ovsCliDir+"/ovs-vsctl", cmdParams...); err != nil {
			return fmt.Errorf("error->%v, adding port to ovs bridge %s", err.Error(), brName)
		}
		log.WithField("portName", brPortInterfaceName).Infof("port added to ovs bridge %s", brName)
		if brName == brPhy1Name {
			ipAddr = ACC_PR_PHY1_IP
		} else if brName == brPhy2Name {
			ipAddr = ACC_PR_PHY2_IP
		}
		if ipAddr == ACC_PR_PHY1_IP || ipAddr == ACC_PR_PHY2_IP {
			cmdParams = []string{"addr", "add", "dev", brName, ipAddr}
			if err := utils.ExecOsCommand("ip", cmdParams...); err != nil {
				return fmt.Errorf("error->%v, assigning IP->%v to ovs bridge %s", err.Error(), ipAddr, brName)
			}
			ipAddr = ""
		}
		cmdParams = []string{"link", "set", "dev", brName, "up"}
		if err := utils.ExecOsCommand("ip", cmdParams...); err != nil {
			return fmt.Errorf("error->%v, bringing UP bridge interface->%v", err.Error(), brName)
		}
	}
	return nil
}

// TODO: Use netlink for ip addr add/ip link set(up) in EnsureBridgeExists/LinkSideBridgeAndPortSetupForF5
func (b *ovsBridge) EnsureBridgeExists() error {
	// ovs-vsctl --may-exist add-br br-vf
	createBrParams := []string{"--may-exist", "add-br", b.brVfName}
	if err := utils.ExecOsCommand(b.ovsCliDir+"/ovs-vsctl", createBrParams...); err != nil {
		return fmt.Errorf("error creating ovs bridge %s with ovs-vsctl command %s", b.brVfName, err.Error())
	}
	//assigning IP for br-vf interface.
	ipAddr := ACC_VM_PR_IP
	cmdParams := []string{"addr", "add", "dev", b.brVfName, ipAddr}
	if err := utils.ExecOsCommand("ip", cmdParams...); err != nil {
		return fmt.Errorf("error->%v, assigning IP->%v to ovs bridge %s", err.Error(), ipAddr, b.brVfName)
	}
	//bring the interface up.
	cmdParams = []string{"link", "set", "dev", b.brVfName, "up"}
	if err := utils.ExecOsCommand("ip", cmdParams...); err != nil {
		return fmt.Errorf("error->%v, bringing UP bridge interface->%v", err.Error(), b.brVfName)
	}
	/*err := LinkSideBridgeAndPortSetupForF5(b)
	if err != nil {
		log.Errorf("error from LinkSideBridgeAndPortSetupForF5->%v\n", err)
		return fmt.Errorf("error from LinkSideBridgeAndPortSetupForF5->%v\n", err)
	}*/
	return nil
}

func DeleteLinkSideBridgeSetupForF5(b *ovsBridge) error {
	var brParams []string
	bridgeNames := []string{brPhy1Name, brPhy2Name}
	for i := 0; i < len(bridgeNames); i++ {
		brParams = []string{"--may-exist", "del-br", bridgeNames[i]}
		if err := utils.ExecOsCommand(b.ovsCliDir+"/ovs-vsctl", brParams...); err != nil {
			log.Infof("Ignoring error->%s, in deleting ovs bridge %s", err.Error(), bridgeNames[i])
		}
	}
	return nil
}

// Note:: This is expected to be called, when plugin exits(Stop),
// so continue to delete, without exiting for any error.
// Note: Deleting bridge, automatically deletes any ports added to it.
func (b *ovsBridge) DeleteBridges() error {
	brParams := []string{"--may-exist", "del-br", b.brVfName}
	if err := utils.ExecOsCommand(b.ovsCliDir+"/ovs-vsctl", brParams...); err != nil {
		log.Errorf("error deleting ovs bridge %s with ovs-vsctl command %s", b.brVfName, err.Error())
	}
	/*err := DeleteLinkSideBridgeSetupForF5(b)
	if err != nil {
		log.Errorf("error from DeleteLinkSideBridgeSetupForF5->%v\n", err)
	}*/
	return nil
}

func (b *ovsBridge) AddPort(portName string) error {
	cmd := exec.Command(b.ovsCliDir+"/ovs-vsctl", "add-port", b.brVfName, portName)
	log.WithField("ovs command", cmd.String()).Debug("adding ovs bridge port")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("unable to add port to the bridge: %w", err)
	}
	log.WithField("portName", portName).Infof("port added to ovs bridge %s", b.brVfName)
	return nil
}

func (b *ovsBridge) DeletePort(portName string) error {
	cmd := exec.Command(b.ovsCliDir+"/ovs-vsctl", "del-port", b.brVfName, portName)
	log.WithField("ovs command", cmd.String()).Debug("deleting ovs bridge port")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("unable to delete port from the bridge: %w", err)
	}
	log.WithField("portName", portName).Infof("port deleted from ovs bridge %s", b.brVfName)
	return nil
}
