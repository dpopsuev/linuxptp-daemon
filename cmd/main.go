package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/openshift/linuxptp-daemon/pkg/config"
	"github.com/openshift/linuxptp-daemon/pkg/daemon"
	"github.com/openshift/linuxptp-daemon/pkg/network"
	ptpv1 "github.com/openshift/ptp-operator/pkg/apis/ptp/v1"

        "k8s.io/client-go/kubernetes"
)

type cliParams struct {
	updateInterval	int
	profileDir	string
}

// Parse Command line flags
func flagInit(cp *cliParams) {
        flag.IntVar(&cp.updateInterval, "update-interval", config.DefaultUpdateInterval,
		"Interval to update PTP status")
        flag.StringVar(&cp.profileDir, "linuxptp-profile-path", config.DefaultProfilePath,
		"profile to start linuxptp processes")
}


func main() {
	cp := &cliParams{}
	flag.Parse()
	flagInit(cp)

	glog.Infof("resync period set to: %d [s]", cp.updateInterval)
	glog.Infof("linuxptp profile path set to: %s", cp.profileDir)

	cfg, err := config.GetKubeConfig()
	if err != nil {
		glog.Errorf("get kubeconfig failed: %v", err)
		return
	}
	glog.Infof("successfully get kubeconfig")

        kubeClient, err := kubernetes.NewForConfig(cfg)
        if err != nil {
                glog.Errorf("cannot create new config for kubeClient: %v", err)
                return
        }

	ptpDevs, err := network.DiscoverPTPDevices()
	if err != nil {
		glog.Errorf("discover PTP devices failed: %v", err)
	}
	glog.Infof("PTP capable NICs: %v", ptpDevs)

	stopCh := make(chan struct{})
	defer close(stopCh)

	nodeName := os.Getenv("NODE_NAME")
	ptpConfUpdate := &daemon.LinuxPTPConfUpdate{UpdateCh: make(chan bool)}
	go daemon.New(
		nodeName,
		daemon.PtpNamespace,
		kubeClient,
		ptpConfUpdate,
		stopCh,
	).Run()

	nodeProfile := filepath.Join(cp.profileDir, nodeName)
	for {
		if _, err := os.Stat(nodeProfile); err != nil {
			if os.IsNotExist(err) {
				glog.Infof("ptp profile doesn't exist for node: %v", nodeName)
				glog.Infof("waiting for 60 seconds ... ")
				time.Sleep(60 * time.Second)
				continue
			} else {
				glog.Fatal("error stating node profile: %v", err)
			}
		}
		break
	}

	var appliedNodeProfileJson []byte
	nodeProfileJson, err := ioutil.ReadFile(nodeProfile)
	if err != nil {
		glog.Fatalf("error reading node profile: %v", nodeProfile)
	}

	ptpConfig := &ptpv1.PtpProfile{}
	err = json.Unmarshal(nodeProfileJson, ptpConfig)
	if err != nil {
		glog.Fatalf("failed to json.Unmarshal ptp profile: %v", err)
	}
	appliedNodeProfileJson = nodeProfileJson
	ptpConfUpdate.NodeProfile = ptpConfig
	ptpConfUpdate.UpdateCh <- true

	tickerPull := time.NewTicker(time.Second * time.Duration(cp.updateInterval))
	defer tickerPull.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	for {
		select {
		case <-tickerPull.C:
			glog.Infof("ticker pull")
			nodeProfileJson, err := ioutil.ReadFile(nodeProfile)
			if err != nil {
				glog.Errorf("error reading node profile: %v", nodeProfile)
				continue
			}
			if string(appliedNodeProfileJson) == string(nodeProfileJson) {
				continue
			}
			err = json.Unmarshal(nodeProfileJson, ptpConfig)
			if err != nil {
				glog.Errorf("failed to json.Unmarshal ptp profile: %v", err)
				continue
			}
			appliedNodeProfileJson = nodeProfileJson
			ptpConfUpdate.NodeProfile = ptpConfig
			ptpConfUpdate.UpdateCh <- true
		case sig := <-sigCh:
			glog.Infof("signal received, shutting down", sig)
			return
		}
	}
}