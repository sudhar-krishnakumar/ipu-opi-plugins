package ipuplugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/intel/ipu-opi-plugins/ipu-plugin/pkg/types"
	pb "github.com/openshift/dpu-operator/dpu-api/gen"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type DevicePluginService struct {
	pb.UnimplementedDeviceServiceServer
	mode string
}

var (
	exclude          = []string{"enp0s1f0", "enp0s1f0d1", "enp0s1f0d2", "enp0s1f0d3"}
	sysClassNet      = "/sys/class/net"
	sysBusPciDevices = "/sys/bus/pci/devices"
	deviceCode       = "0x1452"
	deviceCodeVf     = "0x145c"
	intelVendor      = "0x8086"
	configNumVfs     = 8
)

func NewDevicePluginService(mode string) *DevicePluginService {
	return &DevicePluginService{mode: mode}
}

func (s *DevicePluginService) GetDevices(context.Context, *pb.Empty) (*pb.DeviceListResponse, error) {

	devices, err := discoverHostDevices(s.mode)
	if err != nil {
		return &pb.DeviceListResponse{}, err
	}

	response := &pb.DeviceListResponse{
		Devices: devices,
	}

	fmt.Printf("GetDevices, response->%v\n", response)
	return response, nil
}

func SetNumSriovVfs(pciAddr string, vfCnt int32) error {

	//Note: Upto 64 VFs have been validated.
	if vfCnt <= 0 || vfCnt > 64 {
		return fmt.Errorf("SetNumSriovVfs(): Invalid/unsupported, vfCnt->%v \n", vfCnt)
	}

	pathToNumVfsFile := filepath.Join(sysBusPciDevices, pciAddr, "sriov_numvfs")

	//Need to first write 0 for num of VFs, before updating it.
	err := os.WriteFile(pathToNumVfsFile, []byte("0"), 0644)
	if err != nil {
		return fmt.Errorf("SetNumSriovVfs(): reset fail %s: %v", pathToNumVfsFile, err)
	}

	err = os.WriteFile(pathToNumVfsFile, []byte(strconv.Itoa(int(vfCnt))), 0644)
	if err != nil {
		return fmt.Errorf("SetNumSriovVfs():error in updating %s: %v", pathToNumVfsFile, err)
	}

	// Note: Post-writing, it can take some time for the VFs to be created.
	fmt.Printf("SetNumSriovVfs(): updated file->%s, sriov_numvfs to %v\n", pathToNumVfsFile, vfCnt)

	return nil
}

// Note: Internally...we can have getNumVFs...to check if it is already set.
func SetNumVfs(mode string, numVfs int32) (int32, error) {
	deviceVfsSet := false

	if mode != types.HostMode {
		return 0, fmt.Errorf("setNumVfs(): only supported on host: mode %s", mode)
	}

	//Note: For now setting VFs to hardcoded value.
	fmt.Printf("setNumVfs(): requested VFs->%v, will allocate VFs->%v", numVfs, configNumVfs)
	numVfs = int32(configNumVfs)

	files, err := os.ReadDir(sysBusPciDevices)
	if err != nil {
		return 0, fmt.Errorf("setNumVfs(): error-> %v", err)
	}

	for _, file := range files {
		deviceByte, err := os.ReadFile(filepath.Join(sysBusPciDevices, file.Name(), "device"))
		if err != nil {
			fmt.Printf("Error reading PCIe deviceID: %s\n", err)
			continue
		}

		vendorByte, err := os.ReadFile(filepath.Join(sysBusPciDevices, file.Name(), "vendor"))
		if err != nil {
			fmt.Printf("Error reading VendorID: %s\n", err)
			continue
		}

		deviceId := strings.TrimSpace(string(deviceByte))
		vendorId := strings.TrimSpace(string(vendorByte))

		if deviceId == deviceCode && vendorId == intelVendor {
			err = SetNumSriovVfs(file.Name(), numVfs)
			if err != nil {
				return 0, fmt.Errorf("setNumVfs(): error from SetSriovNumVfs-> %v", err)
			}
			deviceVfsSet = true
		}
	}
	if deviceVfsSet == true {
		return numVfs, nil
	} else {
		return 0, fmt.Errorf("setNumVfs(): unable to set VFs for device->%s", deviceCode)
	}
}

func (s *DevicePluginService) SetNumVfs(ctx context.Context, vfCountReq *pb.VfCount) (*pb.VfCount, error) {

	var res *pb.VfCount
	numVfs, err := SetNumVfs(types.HostMode, vfCountReq.VfCnt)

	fmt.Printf("setNumVfs(): requested VFs->%v, allocated VFs->%v, err->%v", vfCountReq.VfCnt, numVfs, err)
	if err != nil {
		res.VfCnt = 0
	} else {
		res.VfCnt = numVfs
	}

	fmt.Println(res)
	return res, err
}

func discoverHostDevices(mode string) (map[string]*pb.Device, error) {

	devices := make(map[string]*pb.Device)

	files, err := os.ReadDir(sysClassNet)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]*pb.Device), nil
		}
	}

	for _, file := range files {
		deviceCodeByte, err := os.ReadFile(filepath.Join(sysClassNet, file.Name(), "device/device"))
		if err != nil {
			fmt.Printf("Error: %s\n", err)
		}

		device_code := strings.TrimSpace(string(deviceCodeByte))
		if mode == types.IpuMode {
			if device_code == deviceCode {
				if !slices.Contains(exclude, file.Name()) {
					devices[file.Name()] = &pb.Device{ID: file.Name(), Health: pluginapi.Healthy}
				}
			}
		} else if mode == types.HostMode {
			if device_code == deviceCodeVf {
				devices[file.Name()] = &pb.Device{ID: file.Name(), Health: pluginapi.Healthy}
			}
		}
	}
	return devices, nil
}
