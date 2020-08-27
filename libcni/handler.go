// Copyright 2019 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
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

//package cnigrpc
package libcni

import (
	"context"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	//proto "github.com/golang/protobuf/proto"
	//jsonpb "github.com/golang/protobuf/jsonpb"

	//"github.com/containernetworking/cni/libcni"
	//"github.com/mccv1r0/cni/libcni"
	"github.com/containernetworking/cni/pkg/types"
)

const (
	EnvCNIPath = "CNI_PATH"
	EnvNetDir  = "NETCONFPATH"

	DefaultNetDir = "/etc/cni/net.d"
)

// Server represents the gRPC server
type CNIServer struct {
}

func (s *CNIServer) CNIconfig(ctx context.Context, confPath *ConfPath) (*CNIerror, error) {

	cniError := CNIerror{}

	if confPath != nil {
		if confPath.NetDir != "" {
			log.Printf("Receive message NetDir: %s", confPath.NetDir)
		}
		if confPath.NetConf != "" {
			log.Printf("Receive message NetConf: %s", confPath.NetConf)
		}
	}

	log.Printf("Response from server: %v", cniError)
	return &cniError, nil
}

// CNIadd generates result to a CNIaddMsg
func (s *CNIServer) CNIadd(ctx context.Context, in *CNIaddMsg) (*ADDresult, error) {
var f *os.File
var ss string
f, _ = os.OpenFile("/tmp/check.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
defer f.Close()

	log.Printf("Receive message Conf file: %s", in.Conf)
	log.Printf("Receive message ContainerID: %s", in.ContainerID)
	log.Printf("Receive message NetNS: %s", in.NetNS)
	log.Printf("Receive message IfName: %s", in.IfName)
	log.Printf("Receive message CniArgs: %s", in.CniArgs)
	log.Printf("Receive message CniCapArgs: %s", in.CapArgs)

	netconf, rt, cninet, err := cniCommon(in.Conf, in.NetNS, in.IfName, in.CniArgs, in.CapArgs)
	if err != nil {
		return nil, err
	}

	result, err := cninet.AddNetwork(ctx, netconf, rt)
	if err != nil {
		return nil, err
	}
ss = fmt.Sprintf("mcc: Add Result %v of type %T\n", result, result)
_, _ = f.Write([]byte(ss))

	// Need result as []byte  Which is then sent as string() in gRPC result
	data, err := json.MarshalIndent(result, "", "    ")
	if err != nil {
	   return nil, err
	}

	cniResult := ADDresult{}
	if result != nil {
		cniResult.StdOut = string(data)
	}

	log.Printf("Response from server: %s", cniResult.StdOut)
	return &cniResult, nil
}

// CNIcheck generates result to a CNIcheckMsg
func (s *CNIServer) CNIcheck(ctx context.Context, in *CNIcheckMsg) (*CHECKresult, error) {

	log.Printf("Receive message Conf file: %s", in.Conf)
	log.Printf("Receive message ContainerID: %s", in.ContainerID)
	log.Printf("Receive message NetNS: %s", in.NetNS)
	log.Printf("Receive message IfName: %s", in.IfName)
	log.Printf("Receive message CniArgs: %s", in.CniArgs)
	log.Printf("Receive message CniCapArgs: %s", in.CapArgs)

	netconf, rt, cninet, err := cniCommon(in.Conf, in.NetNS, in.IfName, in.CniArgs, in.CapArgs)
	if err != nil {
		return nil, err
	}

	err = cninet.CheckNetwork(ctx, netconf, rt)
	if err != nil {
		return nil, err
	}

	cniResult := CHECKresult{
		Error: "",
	}

	log.Printf("Response from server: %s", cniResult.Error)
	return &cniResult, nil
}

// CNIdel generates result to a CNIdelMsg
func (s *CNIServer) CNIdel(ctx context.Context, in *CNIdelMsg) (*DELresult, error) {

	log.Printf("Receive message Conf file: %s", in.Conf)
	log.Printf("Receive message ContainerID: %s", in.ContainerID)
	log.Printf("Receive message NetNS: %s", in.NetNS)
	log.Printf("Receive message IfName: %s", in.IfName)
	log.Printf("Receive message CniArgs: %s", in.CniArgs)
	log.Printf("Receive message CniCapArgs: %s", in.CapArgs)

	netconf, rt, cninet, err := cniCommon(in.Conf, in.NetNS, in.IfName, in.CniArgs, in.CapArgs)
	if err != nil {
		return nil, err
	}

	err = cninet.DelNetwork(ctx, netconf, rt)
	if err != nil {
		return nil, err
	}

	cniResult := DELresult{
		Error: "",
	}

	log.Printf("Response from server: %s", cniResult.StdOut)
	return &cniResult, nil
}

func parseArgs(args string) ([][2]string, error) {
	var result [][2]string

	pairs := strings.Split(args, ";")
	for _, pair := range pairs {
		kv := strings.Split(pair, "=")
		if len(kv) != 2 || kv[0] == "" || kv[1] == "" {
			return nil, fmt.Errorf("invalid CNI_ARGS pair %q", pair)
		}

		result = append(result, [2]string{kv[0], kv[1]})
	}

	return result, nil
}

type mccNetworkConfig struct {
	Name         string
	CNIVersion   string
	Network *types.NetConf
	Bytes   []byte
}

func cniCommon(conf string, netns string, ifName string, args string, capabilityArgsValue *CNIcapArgs) (*NetworkConfig, *RuntimeConf, *CNIConfig, error) {
var f *os.File
var ss string
f, _ = os.OpenFile("/tmp/check.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
defer f.Close()

	var err error

	log.Printf("cniCommon Called")

	netconf, err := ConfFromBytes([]byte(conf))
	if err != nil {
		return nil, nil, nil, err
	}
ss = fmt.Sprintf("mcc: AddNetwork ConfFromBytes into netconf %v \n", string(netconf.Bytes))
_, _ = f.Write([]byte(ss))

	// Example of how to walk a received protobuf message
	portMappings := capabilityArgsValue.GetPortMappings()
	log.Printf(" portMappings = %v", portMappings)
	for _, portMap := range portMappings {
		log.Printf(" portMap = %v", portMap)
	}

	var capabilityArgs map[string]interface{}
	data, err := json.Marshal(capabilityArgsValue)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := json.Unmarshal(data, &capabilityArgs); err != nil {
		return nil, nil, nil, err
	}

	var cniArgs [][2]string
	if args != "" {
		if len(args) > 0 {
			cniArgs, err = parseArgs(args)
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}

	if ifName == "" {
		ifName = "eth0"
	}

	if netns != "" {
		netns, err = filepath.Abs(netns)
		if err != nil {
			return nil, nil, nil, err
		}
	} else {
		return nil, nil, nil, fmt.Errorf("network namespace is required")
	}

	// Generate the containerid by hashing the netns path
	s := sha512.Sum512([]byte(netns))
	containerID := fmt.Sprintf("cnitool-%x", s[:10])

	//cninet := NewCNIConfig(filepath.SplitList(os.Getenv(EnvCNIPath)), nil)
	mccPath := []string{"/home/mcambria/go/src/github.com/mccv1r0/plugins/bin/"}
	cninet := NewCNIConfig(mccPath, nil)

	rt := &RuntimeConf{
		ContainerID:    containerID,
		NetNS:          netns,
		IfName:         ifName,
		Args:           cniArgs,
		CapabilityArgs: capabilityArgs,
		//CapabilityArgs: portMappings,
	}

ss = fmt.Sprintf("mcc: AddNetwork returns string(netconf.Bytes) %v \n", string(netconf.Bytes))
_, _ = f.Write([]byte(ss))
ss = fmt.Sprintf("mcc: AddNetwork returns rt %v \n", rt)
_, _ = f.Write([]byte(ss))

	//return &netconf, rt, cninet, nil
	return netconf, rt, cninet, nil
}
