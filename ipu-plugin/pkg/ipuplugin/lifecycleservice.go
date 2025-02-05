// Copyright (c) 2024 Intel Corporation.  All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License")
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipuplugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	math_rand "math/rand"
	"net"
	"os"
	"strings"
	"time"

	"github.com/intel/ipu-opi-plugins/ipu-plugin/pkg/p4rtclient"
	"github.com/intel/ipu-opi-plugins/ipu-plugin/pkg/types"
	"github.com/intel/ipu-opi-plugins/ipu-plugin/pkg/utils"
	pb "github.com/openshift/dpu-operator/dpu-api/gen"
	"github.com/pkg/sftp"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type LifeCycleServiceServer struct {
	pb.UnimplementedLifeCycleServiceServer
	daemonHostIp string
	daemonIpuIp  string
	daemonPort   int
	mode         string
	p4rtbin      string
}

const (
	hostVportId         = "03"
	accVportId          = "04"
	deviceId            = "0x1452"
	vendorId            = "0x8086"
	imcAddress          = "192.168.0.1:22"
	apfNumber           = 16
	last_byte_mac_range = 239
)

func NewLifeCycleService(daemonHostIp, daemonIpuIp string, daemonPort int, mode string, p4rtbin string) *LifeCycleServiceServer {
	return &LifeCycleServiceServer{
		daemonHostIp: daemonHostIp,
		daemonIpuIp:  daemonIpuIp,
		daemonPort:   daemonPort,
		mode:         mode,
		p4rtbin:      p4rtbin,
	}
}

type NetworkHandler interface {
	AddrAdd(link netlink.Link, addr *netlink.Addr) error
	AddrList(link netlink.Link, family int) ([]netlink.Addr, error)
	LinkList() ([]netlink.Link, error)
}

type NetworkHandlerImpl struct{}

func (h *NetworkHandlerImpl) AddrAdd(link netlink.Link, addr *netlink.Addr) error {
	return netlink.AddrAdd(link, addr)
}
func (h *NetworkHandlerImpl) AddrList(link netlink.Link, family int) ([]netlink.Addr, error) {
	return netlink.AddrList(link, family)
}
func (h *NetworkHandlerImpl) LinkList() ([]netlink.Link, error) {
	return netlink.LinkList()
}

type FileSystemHandler interface {
	GetDevice(iface string) ([]byte, error)
	GetVendor(iface string) ([]byte, error)
}
type FileSystemHandlerImpl struct{}

func (fs *FileSystemHandlerImpl) GetDevice(iface string) ([]byte, error) {
	return os.ReadFile(fmt.Sprintf("/sys/class/net/%s/device/device", iface))
}
func (fs *FileSystemHandlerImpl) GetVendor(iface string) ([]byte, error) {
	return os.ReadFile(fmt.Sprintf("/sys/class/net/%s/device/vendor", iface))
}

type ExecutableHandler interface {
	validate() bool
	nmcliSetupIpAddress(link netlink.Link, ipStr string, ipAddr *netlink.Addr) error
}

type ExecutableHandlerImpl struct{}

type SSHHandler interface {
	sshFunc() error
}

type SSHHandlerImpl struct{}

type FXPHandler interface {
	configureFXP(p4rtbin string) error
}

type FXPHandlerImpl struct{}

var fileSystemHandler FileSystemHandler
var networkHandler NetworkHandler
var executableHandler ExecutableHandler
var sshHandler SSHHandler
var fxpHandler FXPHandler

func InitHandlers() {
	if fileSystemHandler == nil {
		fileSystemHandler = &FileSystemHandlerImpl{}
	}
	if networkHandler == nil {
		networkHandler = &NetworkHandlerImpl{}
	}
	if executableHandler == nil {
		executableHandler = &ExecutableHandlerImpl{}
	}
	if sshHandler == nil {
		sshHandler = &SSHHandlerImpl{}
	}
	if fxpHandler == nil {
		fxpHandler = &FXPHandlerImpl{}
	}
}

func isPF(iface string) (bool, error) {
	device, err := fileSystemHandler.GetDevice(iface)
	if err != nil {
		return false, fmt.Errorf("cannot identify device with code: %s; error %v ", deviceId, err.Error())
	}

	vendor, err := fileSystemHandler.GetVendor(iface)
	if err != nil {
		return false, fmt.Errorf("cannot identify vendor device with code: %s; error %v ", vendorId, err.Error())
	}

	return strings.TrimSpace(string(device)) == deviceId && strings.TrimSpace(string(vendor)) == vendorId, nil
}

func getCommPf(mode string, linkList []netlink.Link) (netlink.Link, error) {
	var pf netlink.Link
	for i := 0; i < len(linkList); i++ {
		mac := linkList[i].Attrs().HardwareAddr.String()
		octets := strings.Split(mac, ":")

		if mode == types.IpuMode {

			// Check the 4th octet which is used to identify the PF
			if octets[3] == accVportId {

				// On ACC, the 4th octet in the base mac address may already be set to accVportId and used by
				// the another APF (i.e., the first one). If it is the first APF, then it already has an IP.
				// Two distinguish between the two, we select the one which doesn't have an IP set already.
				if list, _ := networkHandler.AddrList(linkList[i], netlink.FAMILY_V4); len(list) == 0 {
					pf = linkList[i]
					break
				}
			}
		} else {

			// Check the 4th octet which is used to identify the PF
			if octets[3] == hostVportId {

				if list, _ := networkHandler.AddrList(linkList[i], netlink.FAMILY_V4); len(list) == 0 {
					pf = linkList[i]
					break
				}
			}
		}
	}

	if pf == nil {
		return nil, fmt.Errorf("check if the ip address already set")
	}

	return pf, nil
}

/*
It can take time for network-manager's state for each interface, to become
activated, when IP address is set, which can cause the IP address to not stick.
TODO: Currently we only support nmcli/NetworkManager daemon combination(RHEL),
this api can be extended for other distros that use different CLI/systemd-networkd.
Option2: First set IP address, sleep for a while, and check
if interface is activated thro nmcli. Retry for few times,
until it succeeds or times out. Also had to add connection,if profile does not exist.
*/
func (e *ExecutableHandlerImpl) nmcliSetupIpAddress(link netlink.Link, ipStr string, ipAddr *netlink.Addr) error {
	var runCmd string
	var err error
	var output string
	maxRetries := 8
	retryInterval := 10 * time.Second
	intfActivated := false
	ipAddrSet := false
	intfName := link.Attrs().Name
	ipWithMask := ipStr + "/24"

	for cnt := 0; cnt < maxRetries; cnt++ {
		if err = networkHandler.AddrAdd(link, ipAddr); err != nil {
			//Note::Can error if address already set, ignoring for now.
			log.Errorf("AddrAdd err ->%v, for ip->%v\n", err, ipAddr)
		}
		addrList, err := networkHandler.AddrList(link, netlink.FAMILY_V4)
		if err == nil {
			ipAddrList := fmt.Sprintf("AddrList->%v\n", addrList)
			if strings.Contains(ipAddrList, ipStr) {
				log.Infof("AddrList->%v, contains expected IP->%v\n", ipAddrList, ipStr)
				ipAddrSet = true
				goto sleep
			}
			log.Errorf("AddrList->%v, does not contain expected IP->%v\n", ipAddrList, ipStr)
		} else {
			log.Errorf("AddrList err ->%v\n", err)
		}
	sleep:
		if ipAddrSet && intfActivated {
			break
		}
		if intfActivated != true {
			time.Sleep(retryInterval)
			output, err = utils.ExecuteScript(`nmcli general status`)
			if err == nil {
				runCmd = fmt.Sprintf(`nmcli -g GENERAL.STATE con show "%s" | grep activated`, intfName)
				output, err = utils.ExecuteScript(runCmd)
				output = strings.TrimSuffix(output, "\n")
				if (output != "activated") || (err != nil) {
					log.Errorf("nmcli err ->%v, output->%v, for cmd->%v\n", err, output, runCmd)
					// no such connection profile
					if strings.Contains(err.Error(), "no such connection profile") {
						runCmd = fmt.Sprintf(`nmcli connection add type ethernet ifname "%s" con-name "%s" \
						ip4 "%s"`, intfName, intfName, ipWithMask)
						_, err = utils.ExecuteScript(runCmd)
						if err != nil {
							log.Errorf("nmcli err->%v, for cmd->%v\n", err, runCmd)
							goto retry
						} else {
							log.Infof("nmcli cmd->%v, passed\n", runCmd)
						}
					}
					goto retry
				} else {
					log.Infof("nmcli interface->%v activated\n", intfName)
					intfActivated = true
				}
			} else {
				log.Infof("network manager not running, err->%v, output-%v\n", err, output)
				goto retry
			}
		}
	retry:
		log.Infof("nmcliSetIPAddress: Retry attempt cnt->%v:\n", cnt)
	}
	if ipAddrSet && intfActivated {
		log.Infof("nmcliSetIPAddress: successful->%v, for interface->%v\n", ipStr, intfName)
		return nil
	}
	log.Errorf("nmcliSetIP: error->%v, setting IP for->%v, ipAddrSet->%v, intfActivated->%v\n", err, intfName, ipAddrSet, intfActivated)
	return fmt.Errorf("nmcliSetIP: error->%v, setting IP for->%v, ipAddrSet->%v, intfActivated->%v\n", err, intfName, ipAddrSet, intfActivated)
}

func setIP(link netlink.Link, ip string) error {
	list, err := networkHandler.AddrList(link, netlink.FAMILY_V4)

	if err != nil {
		log.Errorf("setIP: unable to get the ip address of link: %v\n", err)
		return fmt.Errorf("unable to get the ip address of link: %v", err)
	}

	if len(list) == 0 {

		ipAddr := net.ParseIP(ip)

		if ipAddr.To4() == nil {
			log.Errorf("setIP: invalid ip->%v\n", ipAddr)
			return fmt.Errorf("not a valid IPv4 address: %v", err)
		}

		// Set the IP address on PF
		addr := &netlink.Addr{IPNet: &net.IPNet{IP: ipAddr, Mask: net.CIDRMask(24, 32)}}

		if err = executableHandler.nmcliSetupIpAddress(link, ip, addr); err != nil {
			log.Errorf("setIP: err->%v from nmcliSetup\n", err)
			return fmt.Errorf("setIP: err->%v from nmcliSetup", err)
		}

	} else {
		log.Errorf("address already set. Unset ip address for interface %s and run again\n", link.Attrs().Name)
		return fmt.Errorf("address already set. Unset ip address for interface %s and run again", link.Attrs().Name)
	}
	log.Debugf("setIP: Address->%v, set for interface->%v\n", ip, link.Attrs().Name)
	return nil
}

func GetMacforNetworkInterface(intf string, linkList []netlink.Link) (string, error) {
	mac := ""
	found := false
	for i := 0; i < len(linkList); i++ {
		if linkList[i].Attrs().Name == intf {
			mac = linkList[i].Attrs().HardwareAddr.String()
			log.Debugf("found mac->%v for interface->%v\n", mac, intf)
			found = true
			break
		}
	}

	if found == true {
		return mac, nil
	}
	log.Errorf("Couldnt find mac for interface->%v\n", intf)
	return "", fmt.Errorf("Couldnt find mac for interface->%v\n", intf)
}

func GetFilteredPFs(pfList *[]netlink.Link) error {

	linkList, err := networkHandler.LinkList()

	if err != nil || len(linkList) == 0 {
		return fmt.Errorf("unable to retrieve link list: %v, len->%v", err, len(linkList))
	}

	for i := 0; i < len(linkList); i++ {
		result, err := isPF(linkList[i].Attrs().Name)

		if result && err == nil {
			*pfList = append(*pfList, linkList[i])
		}
	}

	return nil
}

/*
If IDPF net devices dont show up on host-side(this can happen if IMC reboot is done without rmmod(for IDPF on host).
This function is a best effort to bring-up IDPF netdevices, using rmmod/modprobe of IDPF.
*/
func checkIdpfNetDevices(mode string) {
	var pfList []netlink.Link
	if mode == types.HostMode {
		if err := GetFilteredPFs(&pfList); err != nil {
			log.Errorf("checkNetDevices: err->%v from GetFilteredPFs", err)
			return
		}
		//Case where we dont see host IDPF netdevices.
		if len(pfList) == 0 {
			log.Debugf("Not seeing host IDPF netdevices, attempt rmmod/modprobe\n")
			output, err := utils.ExecuteScript(`lsmod | grep idpf`)
			if err != nil {
				log.Errorf("lsmod err->%v, output->%v\n", err, output)
				return
			}

			_, err = utils.ExecuteScript(`rmmod idpf`)

			if err != nil {
				log.Errorf("rmmod err->%v\n", err)
				return
			} else {
				_, err = utils.ExecuteScript(`modprobe idpf`)
				if err != nil {
					log.Errorf("modprobe err->%v\n", err)
					return
				}
			}
			log.Debugf("completed-rmmod and modprobe of IDPF\n")
		} else {
			log.Debugf("host IDPF netdevices exist, count->%d\n", len(pfList))
		}
	}
}

func configureChannel(mode, daemonHostIp, daemonIpuIp string) error {

	var pfList []netlink.Link

	if err := GetFilteredPFs(&pfList); err != nil {
		fmt.Printf("configureChannel: err->%v from GetFilteredPFs", err)
		return status.Error(codes.Internal, err.Error())
	}

	pf, err := getCommPf(mode, pfList)

	if pf == nil {
		// Address already set - we don't proceed with setting the ip
		fmt.Printf("configureChannel: pf nil from getCommPf\n")
		return nil
	}

	if err != nil {
		fmt.Printf("configureChannel: err->%v from getCommPf\n", err)
		return status.Error(codes.Internal, err.Error())
	}

	var ip string

	if mode == "ipu" {
		ip = daemonIpuIp
	} else {
		ip = daemonHostIp
	}

	if err := setIP(pf, ip); err != nil {
		fmt.Printf("configureChannel: err->%v from setIP", err)
		return status.Error(codes.Internal, err.Error())
	}

	return nil
}

// sets random bytes for last 2 bytes(5th and 6th) in MAC address
func setBaseMacAddr() (string, error) {
	var macAddress string
	macBytes := make([]byte, 2)
	_, err := rand.Read(macBytes)
	if err != nil {
		return "", fmt.Errorf("error->%v, failed to create random bytes for MAC: ", err)
	}
	//Restricting range of last byte in node-policy to be less than 240,
	//to allow for 16 function-ids. Since last-byte(+1) is done
	//for the 16 function-ids, in CP code->set_start_mac_address(in mac_utils.c)
	if macBytes[1] > last_byte_mac_range {
		macBytes[1] = byte(math_rand.Intn(last_byte_mac_range) + 1)
	}
	log.Debugf("mac bytes ->%v\n", macBytes)

	macAddress = fmt.Sprintf("00:00:00:00:%x:%x", macBytes[0], macBytes[1])
	log.Info("Allocated IPU MAC pattern:", macAddress)

	return macAddress, nil
}

func (s *SSHHandlerImpl) sshFunc() error {
	config := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password(""),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// Connect to the remote server.
	client, err := ssh.Dial("tcp", imcAddress, config)
	if err != nil {
		return fmt.Errorf("failed to dial: %s", err)
	}
	defer client.Close()

	// Create an SFTP client.
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("failed to create SFTP client: %s", err)
	}
	defer sftpClient.Close()

	// Open the source file.
	localFilePath := "/rh_mvp.pkg"
	srcFile, err := os.Open(localFilePath)
	if err != nil {
		return fmt.Errorf("failed to open local file: %s", err)
	}
	defer srcFile.Close()

	// Create the destination file on the remote server.
	remoteFilePath := "/work/scripts/rh_mvp.pkg"
	dstFile, err := sftpClient.Create(remoteFilePath)
	if err != nil {
		return fmt.Errorf("failed to create remote file: %s", err)
	}
	defer dstFile.Close()

	// Copy the file contents to the destination file.
	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return fmt.Errorf("failed to copy file: %s", err)
	}

	// Ensure that the file is written to the remote filesystem.
	err = dstFile.Sync()
	if err != nil {
		return fmt.Errorf("failed to sync file: %s", err)
	}

	// Start a session.
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %s", err)
	}
	defer session.Close()

	// Append python script to configure the ACC
	commands := `echo "python /usr/bin/scripts/cfg_acc_apf_x2.py" >> /work/scripts/pre_init_app.sh`
	err = session.Run(commands)
	if err != nil {
		return fmt.Errorf("failed to run commands: %s", err)
	}

	macAddress, err := setBaseMacAddr()
	if err != nil {
		return fmt.Errorf("error from setBaseMacAddr()->%v", err)
	}

	shellScript := fmt.Sprintf(`#!/bin/sh
CP_INIT_CFG=/etc/dpcp/cfg/cp_init.cfg
echo "Checking for custom package..."
if [ -e rh_mvp.pkg ]; then
    echo "Custom package rh_mvp.pkg found. Overriding default package"
    cp rh_mvp.pkg /etc/dpcp/package/
    rm -rf /etc/dpcp/package/default_pkg.pkg
    ln -s /etc/dpcp/package/rh_mvp.pkg /etc/dpcp/package/default_pkg.pkg
    sed -i 's/sem_num_pages = 1;/sem_num_pages = 25;/g' $CP_INIT_CFG
    sed -i 's/pf_mac_address = "00:00:00:00:03:14";/pf_mac_address = "%s";/g' $CP_INIT_CFG
    sed -i 's/acc_apf = 4;/acc_apf = 16;/g' $CP_INIT_CFG
    sed -i 's/comm_vports = .*/comm_vports = ((\[5,0\],\[4,0\]),(\[0,3\],\[4,4\]));/g' $CP_INIT_CFG
else
    echo "No custom package found. Continuing with default package"
fi
`, macAddress) // Insert the MAC address variable into the script.

	loadCustomPkgFilePath := "/work/scripts/load_custom_pkg.sh"
	loadCustomPkgFile, err := sftpClient.Create(loadCustomPkgFilePath)
	if err != nil {
		return fmt.Errorf("failed to create remote load_custom_pkg.sh: %s", err)
	}
	defer loadCustomPkgFile.Close()

	_, err = loadCustomPkgFile.Write([]byte(shellScript))
	if err != nil {
		return fmt.Errorf("failed to write to load_custom_pkg.sh: %s", err)
	}

	err = loadCustomPkgFile.Sync()
	if err != nil {
		return fmt.Errorf("failed to sync load_custom_pkg.sh: %s", err)
	}

	uuidFilePath := "/work/uuid"
	uuidFile, err := sftpClient.Create(uuidFilePath)
	if err != nil {
		return fmt.Errorf("failed to create remote uuid file: %s", err)
	}
	defer uuidFile.Close()

	// Write the new MAC address to the uuid file.
	_, err = uuidFile.Write([]byte(macAddress + "\n"))
	if err != nil {
		return fmt.Errorf("failed to write to uuid file: %s", err)
	}

	// Ensure that the uuid file is written to the remote filesystem.
	err = uuidFile.Sync()
	if err != nil {
		return fmt.Errorf("failed to sync uuid file: %s", err)
	}

	session, err = client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %s", err)
	}
	defer session.Close()

	// Run a command on the remote server and capture the output.
	var stdoutBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	err = session.Run(commands)
	if err != nil {
		return fmt.Errorf("failed to run commands: %s", err)
	}

	session, err = client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %s", err)
	}
	defer session.Close()

	err = session.Run("reboot")
	if err != nil {
		return fmt.Errorf("failed to run commands: %s", err)
	}

	return nil
}

func countAPFDevices() int {
	var pfList []netlink.Link

	if err := GetFilteredPFs(&pfList); err != nil {
		return 0
	}

	return len(pfList)
}

func checkIfMACIsSet() (bool, string) {
	config := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password(""),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// Connect to the remote server.
	client, err := ssh.Dial("tcp", imcAddress, config)
	if err != nil {
		return false, fmt.Sprintf("failed to dial remote server: %s", err)
	}
	defer client.Close()

	// Start a session.
	session, err := client.NewSession()
	if err != nil {
		return false, fmt.Sprintf("failed to create session: %s", err)
	}
	defer session.Close()

	commands := "if [ -f /work/uuid ]; then echo 'File exists'; else echo 'File does not exist'; fi"

	// Run a command on the remote server and capture the output.
	var stdoutBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	err = session.Run(commands)
	if err != nil {
		return false, fmt.Sprintf("mac not found: %s", err)
	}

	output := stdoutBuf.String()
	if output == "File exists\n" {
		return true, "File exists"
	} else {
		return false, "File does not exist"
	}
}

func (e *ExecutableHandlerImpl) validate() bool {

	if numAPFs := countAPFDevices(); numAPFs < apfNumber {
		fmt.Printf("Not enough APFs %v", numAPFs)
		return false
	}

	if macPreFix, mac := checkIfMACIsSet(); !macPreFix {
		fmt.Printf("incorrect Mac assigned : %v\n", mac)
		return false
	}

	return true
}

func (s *FXPHandlerImpl) configureFXP(p4rtbin string) error {
	vfMacList, err := utils.GetVfMacList()

	if err != nil {
		return fmt.Errorf("unable to reach the IMC %v", err)
	}

	if len(vfMacList) == 0 {
		return fmt.Errorf("no NFs initialized on the host")
	}

	p4rtclient.DeletePointToPointVFRules(p4rtbin, vfMacList)
	p4rtclient.CreatePointToPointVFRules(p4rtbin, vfMacList)

	return nil
}

func (s *LifeCycleServiceServer) Init(ctx context.Context, in *pb.InitRequest) (*pb.IpPort, error) {
	InitHandlers()

	if in.DpuMode && s.mode != types.IpuMode || !in.DpuMode && s.mode != types.HostMode {
		return nil, status.Errorf(codes.Internal, "Ipu plugin running in %s mode", s.mode)
	}

	if in.DpuMode {
		if val := executableHandler.validate(); !val {
			log.Info("forcing state")
			if err := sshHandler.sshFunc(); err != nil {
				return nil, fmt.Errorf("error calling sshFunc %s", err)
			}
		} else {
			log.Info("not forcing state")
		}

		// Preconfigure the FXP with point-to-point rules between host VFs
		if err := fxpHandler.configureFXP(s.p4rtbin); err != nil {
			return nil, status.Errorf(codes.Internal, "Error when preconfiguring the FXP: %v", err)
		}
	}

	checkIdpfNetDevices(s.mode)

	if err := configureChannel(s.mode, s.daemonHostIp, s.daemonIpuIp); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	response := &pb.IpPort{Ip: s.daemonIpuIp, Port: int32(s.daemonPort)}

	return response, nil
}
