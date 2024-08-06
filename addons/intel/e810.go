package intel

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"github.com/openshift/linuxptp-daemon/pkg/dpll"
	"github.com/openshift/linuxptp-daemon/pkg/plugin"
	ptpv1 "github.com/openshift/ptp-operator/api/v1"
)

type E810Opts struct {
	EnableDefaultConfig bool                         `json:"enableDefaultConfig"`
	UblxCmds            []E810UblxCmds               `json:"ublxCmds"`
	DevicePins          map[string]map[string]string `json:"pins"`
	DpllSettings        map[string]uint64            `json:"settings"`
	PhaseOffsetPins     map[string]map[string]string `json:"phaseOffsetPins"`
}

type E810UblxCmds struct {
	ReportOutput bool     `json:"reportOutput"`
	Args         []string `json:"args"`
}

type E810PluginData struct {
	hwplugins *[]string
}

// Sourced from https://github.com/RHsyseng/oot-ice/blob/main/ptp-config.sh
var EnableE810PTPConfig = `
#!/bin/bash
set -eu

ETH=$(grep -e 000e -e 000f /sys/class/net/*/device/subsystem_device | awk -F"/" '{print $5}')

for DEV in $ETH; do
  if [ -f /sys/class/net/$DEV/device/ptp/ptp*/pins/U.FL2 ]; then
    echo 0 2 > /sys/class/net/$DEV/device/ptp/ptp*/pins/U.FL2
    echo 0 1 > /sys/class/net/$DEV/device/ptp/ptp*/pins/U.FL1
    echo 0 2 > /sys/class/net/$DEV/device/ptp/ptp*/pins/SMA2
    echo 0 1 > /sys/class/net/$DEV/device/ptp/ptp*/pins/SMA1
  fi
done

echo "Disabled all SMA and U.FL Connections"
`

func getDefaultUblxCmds() []E810UblxCmds {
	// Ublx command to output NAV-CLOCK every second
	cfgMsgNavClock := E810UblxCmds{
		ReportOutput: false,
		Args:         []string{"-p", "CFG-MSG,1,34,1"},
	}
	// Ublx command to output NAV-STATUS every second
	cfgMsgNavStatus := E810UblxCmds{
		ReportOutput: false,
		Args:         []string{"-p", "CFG-MSG,1,3,1"},
	}

	// Ublx command to save configuration to storage
	cfgSave := E810UblxCmds{
		ReportOutput: false,
		Args:         []string{"-p", "SAVE"},
	}
	return []E810UblxCmds{cfgMsgNavClock, cfgMsgNavStatus, cfgSave}
}

func OnPTPConfigChangeE810(data *interface{}, nodeProfile *ptpv1.PtpProfile) error {
	glog.Info("calling onPTPConfigChange for e810 plugin")
	var e810Opts E810Opts
	var err error
	var optsByteArray []byte
	var stdout []byte
	var pinPath string

	e810Opts.EnableDefaultConfig = false

	for name, opts := range (*nodeProfile).Plugins {
		if name == "e810" {
			optsByteArray, _ = json.Marshal(opts)
			err = json.Unmarshal(optsByteArray, &e810Opts)
			if err != nil {
				glog.Error("e810 failed to unmarshal opts: " + err.Error())
			}
			if e810Opts.EnableDefaultConfig {
				stdout, _ = exec.Command("/usr/bin/bash", "-c", EnableE810PTPConfig).Output()
				glog.Infof(string(stdout))
			}
			if (*nodeProfile).PtpSettings == nil {
				(*nodeProfile).PtpSettings = make(map[string]string)
			}
			for device, pins := range e810Opts.DevicePins {
				dpllClockIdStr := fmt.Sprintf("%s[%s]", dpll.ClockIdStr, device)
				// for unit testing only, PtpSettings may include "unitTest" key. The value is
				// the path where resulting configuration files will be written, instead of /var/run
				if _, found := (*nodeProfile).PtpSettings["unitTest"]; found {
					buf := make([]byte, 8)
					rand.Read(buf)
					(*nodeProfile).PtpSettings[dpllClockIdStr] = strconv.FormatUint(binary.LittleEndian.Uint64(buf), 10)
				} else {
					(*nodeProfile).PtpSettings[dpllClockIdStr] = strconv.FormatUint(getClockIdE810(device), 10)
				}
				for pin, value := range pins {
					deviceDir := fmt.Sprintf("/sys/class/net/%s/device/ptp/", device)
					phcs, err := os.ReadDir(deviceDir)
					if err != nil {
						glog.Error("e810 failed to read " + deviceDir + ": " + err.Error())
						continue
					}

					for _, phc := range phcs {
						if strings.HasPrefix(pin, "SDP") {
							pinPath = fmt.Sprintf("/sys/class/net/%s/device/ptp/%s/period", device, phc.Name())
						} else {
							pinPath = fmt.Sprintf("/sys/class/net/%s/device/ptp/%s/pins/%s", device, phc.Name(), pin)
						}
						glog.Infof("echo %s > %s", value, pinPath)
						err = os.WriteFile(pinPath, []byte(value), 0666)
						if err != nil {
							glog.Error("e810 failed to write " + value + " to " + pinPath + ": " + err.Error())
						}
					}
				}
			}
			for k, v := range e810Opts.DpllSettings {
				if _, ok := (*nodeProfile).PtpSettings[k]; !ok {
					(*nodeProfile).PtpSettings[k] = strconv.FormatUint(v, 10)
				}
			}
			for iface, properties := range e810Opts.PhaseOffsetPins {
				ifaceFound := false
				for dev := range e810Opts.DevicePins {
					if strings.Compare(iface, dev) == 0 {
						ifaceFound = true
						break
					}
				}
				if !ifaceFound {
					glog.Errorf("e810 phase offset pin filter initialization failed: interface %s not found among  %v",
						iface, reflect.ValueOf(e810Opts.DevicePins).MapKeys())
					break
				}
				for pinProperty, value := range properties {
					key := strings.Join([]string{iface, "phaseOffsetFilter", strconv.FormatUint(getClockIdE810(iface), 10), pinProperty}, ".")
					(*nodeProfile).PtpSettings[key] = value
				}
			}
		}
	}
	return nil
}

func AfterRunPTPCommandE810(data *interface{}, nodeProfile *ptpv1.PtpProfile, command string) error {
	glog.Info("calling AfterRunPTPCommandE810 for e810 plugin")
	var e810Opts E810Opts
	var err error
	var optsByteArray []byte
	var stdout []byte

	e810Opts.EnableDefaultConfig = false

	for name, opts := range (*nodeProfile).Plugins {
		if name == "e810" {
			optsByteArray, _ = json.Marshal(opts)
			err = json.Unmarshal(optsByteArray, &e810Opts)
			if err != nil {
				glog.Error("e810 failed to unmarshal opts: " + err.Error())
			}
			if command == "gpspipe" {
				glog.Infof("AfterRunPTPCommandE810 doing ublx config for command: %s", command)
				for _, ublxOpt := range append(e810Opts.UblxCmds, getDefaultUblxCmds()...) {
					ublxArgs := ublxOpt.Args
					glog.Infof("Running /usr/bin/ubxtool with args %s", strings.Join(ublxArgs, ", "))
					stdout, err = exec.Command("/usr/local/bin/ubxtool", ublxArgs...).CombinedOutput()
					//stdout, err = exec.Command("/usr/local/bin/ubxtool", "-p", "STATUS").CombinedOutput()
					_data := *data
					if data != nil && ublxOpt.ReportOutput {
						glog.Infof("Saving status to hwconfig: %s", string(stdout))
						var pluginData *E810PluginData = _data.(*E810PluginData)
						_pluginData := *pluginData
						statusString := fmt.Sprintf("ublx data: %s", string(stdout))
						*_pluginData.hwplugins = append(*_pluginData.hwplugins, statusString)
					} else {
						glog.Infof("Not saving status to hwconfig: %s", string(stdout))
					}
				}
			} else {
				glog.Infof("AfterRunPTPCommandE810 doing nothing for command: %s", command)
			}
		}
	}
	return nil
}

func PopulateHwConfigE810(data *interface{}, hwconfigs *[]ptpv1.HwConfig) error {
	//hwConfig := ptpv1.HwConfig{}
	//hwConfig.DeviceID = "e810"
	//*hwconfigs = append(*hwconfigs, hwConfig)
	if data != nil {
		_data := *data
		var pluginData *E810PluginData = _data.(*E810PluginData)
		_pluginData := *pluginData
		if _pluginData.hwplugins != nil {
			for _, _hwconfig := range *_pluginData.hwplugins {
				hwConfig := ptpv1.HwConfig{}
				hwConfig.DeviceID = "e810"
				hwConfig.Status = _hwconfig
				*hwconfigs = append(*hwconfigs, hwConfig)
			}
		}
	}
	return nil
}

func E810(name string) (*plugin.Plugin, *interface{}) {
	if name != "e810" {
		glog.Errorf("Plugin must be initialized as 'e810'")
		return nil, nil
	}
	glog.Infof("registering e810 plugin")
	hwplugins := []string{}
	pluginData := E810PluginData{hwplugins: &hwplugins}
	_plugin := plugin.Plugin{Name: "e810",
		OnPTPConfigChange:  OnPTPConfigChangeE810,
		AfterRunPTPCommand: AfterRunPTPCommandE810,
		PopulateHwConfig:   PopulateHwConfigE810,
	}
	var iface interface{} = &pluginData
	return &_plugin, &iface
}

func getClockIdE810(device string) uint64 {
	const (
		PCI_EXT_CAP_ID_DSN       = 3
		PCI_CFG_SPACE_SIZE       = 256
		PCI_EXT_CAP_NEXT_OFFSET  = 2
		PCI_EXT_CAP_OFFSET_SHIFT = 4
		PCI_EXT_CAP_DATA_OFFSET  = 4
	)
	b, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/device/config", device))
	if err != nil {
		glog.Error(err)
		return 0
	}
	// Extended capability space starts right on PCI_CFG_SPACE
	var offset uint16 = PCI_CFG_SPACE_SIZE
	var id uint16
	for {
		id = binary.LittleEndian.Uint16(b[offset:])
		if id != PCI_EXT_CAP_ID_DSN {
			if id == 0 {
				glog.Errorf("can't find DSN for device %s", device)
				return 0
			}
			offset = binary.LittleEndian.Uint16(b[offset+PCI_EXT_CAP_NEXT_OFFSET:]) >> PCI_EXT_CAP_OFFSET_SHIFT
			continue
		}
		break
	}
	return binary.LittleEndian.Uint64(b[offset+PCI_EXT_CAP_DATA_OFFSET:])
}
