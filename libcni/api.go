// Copyright 2015 CNI authors
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

package libcni

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"log"
	"bytes"
	"net"
	"syscall"

	//"github.com/mccv1r0/cni/cnigrpc"
	//proto "github.com/golang/protobuf/proto"
	"google.golang.org/grpc"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
)

const (
	unixSocketPath = "/tmp/grpc.sock"
)

var (
	CacheDir = "/var/lib/cni"
)

// A RuntimeConf holds the arguments to one invocation of a CNI plugin
// excepting the network configuration, with the nested exception that
// the `runtimeConfig` from the network configuration is included
// here.
type RuntimeConf struct {
	ContainerID string
	NetNS       string
	IfName      string
	Args        [][2]string
	// A dictionary of capability-specific data passed by the runtime
	// to plugins as top-level keys in the 'runtimeConfig' dictionary
	// of the plugin's stdin data.  libcni will ensure that only keys
	// in this map which match the capabilities of the plugin are passed
	// to the plugin
	CapabilityArgs map[string]interface{}

	// A cache directory in which to library data.  Defaults to CacheDir
	CacheDir string
}

type NetworkConfig struct {
	Network *types.NetConf
	Bytes   []byte
}

type NetworkConfigList struct {
	Name         string
	CNIVersion   string
	DisableCheck bool
	Plugins      []*NetworkConfig
	Bytes        []byte
}

type CNI interface {
	AddNetworkList(ctx context.Context, net *NetworkConfigList, rt *RuntimeConf) (types.Result, error)
	CheckNetworkList(ctx context.Context, net *NetworkConfigList, rt *RuntimeConf) error
	DelNetworkList(ctx context.Context, net *NetworkConfigList, rt *RuntimeConf) error

	AddNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) (types.Result, error)
	CheckNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) error
	DelNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) error
	GetNetworkCachedResult(net *NetworkConfig, rt *RuntimeConf) (types.Result, error)

	ValidateNetworkList(ctx context.Context, net *NetworkConfigList) ([]string, error)
	ValidateNetwork(ctx context.Context, net *NetworkConfig) ([]string, error)
}

type CNIConfig struct {
	Path []string
	exec invoke.Exec
	ClientgRPC bool
	Conn *grpc.ClientConn
}

// CNIConfig implements the CNI interface
var _ CNI = &CNIConfig{}

// NewCNIConfig returns a new CNIConfig object that will search for plugins
// in the given paths and use the given exec interface to run those plugins,
// or if the exec interface is not given, will use a default exec handler.
func NewCNIConfig(path []string, exec invoke.Exec) *CNIConfig {
	return &CNIConfig{
		Path: path,
		exec: exec,
		ClientgRPC: false,
	}
}

func stringFromArgs(pairs [][2]string) (string, error) {
	var b bytes.Buffer

	for _, pair := range pairs {
		b.WriteString(pair[0])
		b.WriteString("=")
		b.WriteString(pair[1])
		b.WriteString(";")
	}

	return b.String(), nil
}

func buildOneConfig(name, cniVersion string, orig *NetworkConfig, prevResult types.Result, rt *RuntimeConf) (*NetworkConfig, error) {
	var err error

	inject := map[string]interface{}{
		"name":       name,
		"cniVersion": cniVersion,
	}
	// Add previous plugin result
	if prevResult != nil {
		inject["prevResult"] = prevResult
	}

	// Ensure every config uses the same name and version
	orig, err = InjectConf(orig, inject)
	if err != nil {
		return nil, err
	}

	return injectRuntimeConfig(orig, rt)
}

// This function takes a libcni RuntimeConf structure and injects values into
// a "runtimeConfig" dictionary in the CNI network configuration JSON that
// will be passed to the plugin on stdin.
//
// Only "capabilities arguments" passed by the runtime are currently injected.
// These capabilities arguments are filtered through the plugin's advertised
// capabilities from its config JSON, and any keys in the CapabilityArgs
// matching plugin capabilities are added to the "runtimeConfig" dictionary
// sent to the plugin via JSON on stdin.  For example, if the plugin's
// capabilities include "portMappings", and the CapabilityArgs map includes a
// "portMappings" key, that key and its value are added to the "runtimeConfig"
// dictionary to be passed to the plugin's stdin.
func injectRuntimeConfig(orig *NetworkConfig, rt *RuntimeConf) (*NetworkConfig, error) {
	var err error

	rc := make(map[string]interface{})
	for capability, supported := range orig.Network.Capabilities {
		if !supported {
			continue
		}
		if data, ok := rt.CapabilityArgs[capability]; ok {
			rc[capability] = data
		}
	}

	if len(rc) > 0 {
		orig, err = InjectConf(orig, map[string]interface{}{"runtimeConfig": rc})
		if err != nil {
			return nil, err
		}
	}

	return orig, nil
}

// ensure we have a usable exec if the CNIConfig was not given one
func (c *CNIConfig) ensureExec() invoke.Exec {
	if c.exec == nil {
		c.exec = &invoke.DefaultExec{
			RawExec:       &invoke.RawExec{Stderr: os.Stderr},
			PluginDecoder: version.PluginDecoder{},
		}
	}
	return c.exec
}

func getResultCacheFilePath(netName string, rt *RuntimeConf) string {
	cacheDir := rt.CacheDir
	if cacheDir == "" {
		cacheDir = CacheDir
	}
	return filepath.Join(cacheDir, "results", fmt.Sprintf("%s-%s-%s", netName, rt.ContainerID, rt.IfName))
}

func setCachedResult(result types.Result, netName string, rt *RuntimeConf) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	fname := getResultCacheFilePath(netName, rt)
	if err := os.MkdirAll(filepath.Dir(fname), 0700); err != nil {
		return err
	}
	return ioutil.WriteFile(fname, data, 0600)
}

func delCachedResult(netName string, rt *RuntimeConf) error {
	fname := getResultCacheFilePath(netName, rt)
	return os.Remove(fname)
}

func getCachedResult(netName, cniVersion string, rt *RuntimeConf) (types.Result, error) {
	fname := getResultCacheFilePath(netName, rt)
	data, err := ioutil.ReadFile(fname)
	if err != nil {
		// Ignore read errors; the cached result may not exist on-disk
		return nil, nil
	}

	// Read the version of the cached result
	decoder := version.ConfigDecoder{}
	resultCniVersion, err := decoder.Decode(data)
	if err != nil {
		return nil, err
	}

	// Ensure we can understand the result
	result, err := version.NewResult(resultCniVersion, data)
	if err != nil {
		return nil, err
	}

	// Convert to the config version to ensure plugins get prevResult
	// in the same version as the config.  The cached result version
	// should match the config version unless the config was changed
	// while the container was running.
	result, err = result.GetAsVersion(cniVersion)
	if err != nil && resultCniVersion != cniVersion {
		return nil, fmt.Errorf("failed to convert cached result version %q to config version %q: %v", resultCniVersion, cniVersion, err)
	}
	return result, err
}

// GetNetworkListCachedResult returns the cached Result of the previous
// previous AddNetworkList() operation for a network list, or an error.
func (c *CNIConfig) GetNetworkListCachedResult(list *NetworkConfigList, rt *RuntimeConf) (types.Result, error) {
	return getCachedResult(list.Name, list.CNIVersion, rt)
}

// GetNetworkCachedResult returns the cached Result of the previous
// previous AddNetwork() operation for a network, or an error.
func (c *CNIConfig) GetNetworkCachedResult(net *NetworkConfig, rt *RuntimeConf) (types.Result, error) {
	return getCachedResult(net.Network.Name, net.Network.CNIVersion, rt)
}

func (c *CNIConfig) addNetwork(ctx context.Context, name, cniVersion string, net *NetworkConfig, prevResult types.Result, rt *RuntimeConf) (types.Result, error) {
var f *os.File
var s string
f, _ = os.OpenFile("/tmp/check.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
defer f.Close()

if c.ClientgRPC {
   s = fmt.Sprintf("mcc: addNetwork Called as CLIENT: name %v\n", name)
   _, _ = f.Write([]byte(s))
} else {
   s = fmt.Sprintf("mcc: addNetwork Called as SERVER: name %v\n", name)
   _, _ = f.Write([]byte(s))
}
	c.ensureExec()
	pluginPath, err := c.exec.FindInPath(net.Network.Type, c.Path)
	if err != nil {
		return nil, err
	}

	newConf, err := buildOneConfig(name, cniVersion, net, prevResult, rt)
	if err != nil {
		return nil, err
	}

	capabilityArgs := CNIcapArgs{}
	if rt.CapabilityArgs != nil {
	   data, err := json.Marshal(rt.CapabilityArgs)
	   capabilityArgsValue := string(data)
	   if len(capabilityArgsValue) > 0 {
		//println("capabilityArgsValue: ", capabilityArgsValue)
		s = fmt.Sprintf("mcc: capabilityArgsValue: %v of type %T \n", capabilityArgsValue, capabilityArgsValue)
		_, _ = f.Write([]byte(s))
		if err = json.Unmarshal([]byte(capabilityArgsValue), &capabilityArgs); err != nil {
			return nil, err
		}
		s = fmt.Sprintf("mcc: capabilityArgs: %v of type %T \n", capabilityArgs, capabilityArgs)
		_, _ = f.Write([]byte(s))
	   }
	}

	var cniArgs string
	if len(rt.Args) > 0 {
		cniArgs, _ = stringFromArgs(rt.Args)
		s = fmt.Sprintf("mcc: cniArgs: %v of type %T \n", cniArgs, cniArgs)
		_, _ = f.Write([]byte(s))
	}

	if !c.ClientgRPC {
	   return invoke.ExecPluginWithResult(ctx, pluginPath, newConf.Bytes, c.args("ADD", rt), c.exec)
	} else {
	   //err, resultString := gRPCsendAdd(ctx, c.Conn, string(newConf.Bytes), rt.NetNS, rt.IfName, rt.Args, rt.CapabilityArgs)
	   err, resultString := gRPCsendAdd(ctx, c.Conn, string(newConf.Bytes), rt.NetNS, rt.IfName, cniArgs, capabilityArgs)
	   if err != nil {
		return nil, err
	   }

	   // Plugin must return result in same version as specified in netconf
	   versionDecoder := &version.ConfigDecoder{}
	   confVersion, err := versionDecoder.Decode(newConf.Bytes)
	   if err != nil {
		return nil, err
	   }

	   return version.NewResult(confVersion, []byte(resultString))
	}
	return nil, err
}

// AddNetworkList executes a sequence of plugins with the ADD command
func (c *CNIConfig) AddNetworkList(ctx context.Context, list *NetworkConfigList, rt *RuntimeConf) (types.Result, error) {
var f *os.File
var s string
f, _ = os.OpenFile("/tmp/check.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
defer f.Close()
	var err error
	var result types.Result

	for _, net := range list.Plugins {
s = fmt.Sprintf("  mcc: AddNetworkLIST net.Network.Type (plugin name) %v\n", net.Network.Type)
_, _ = f.Write([]byte(s))
		result, err = c.addNetwork(ctx, list.Name, list.CNIVersion, net, result, rt)
		if err != nil {
			return nil, err
		}
	}

	if err = setCachedResult(result, list.Name, rt); err != nil {
		return nil, fmt.Errorf("failed to set network %q cached result: %v", list.Name, err)
	}

	return result, nil
}

func (c *CNIConfig) checkNetwork(ctx context.Context, name, cniVersion string, net *NetworkConfig, prevResult types.Result, rt *RuntimeConf) error {
var f *os.File
var s string
f, _ = os.OpenFile("/tmp/check.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
defer f.Close()

if c.ClientgRPC {
   s = fmt.Sprintf("mcc: checkNetwork Called as CLIENT:\n")
   _, _ = f.Write([]byte(s))
} else {
   s = fmt.Sprintf("mcc: checkNetwork Called as SERVER:\n")
   _, _ = f.Write([]byte(s))
}
	c.ensureExec()
	pluginPath, err := c.exec.FindInPath(net.Network.Type, c.Path)
	if err != nil {
		return err
	}

	newConf, err := buildOneConfig(name, cniVersion, net, prevResult, rt)
	if err != nil {
		return err
	}

	capabilityArgs := CNIcapArgs{}
	if rt.CapabilityArgs != nil {
	   data, err := json.Marshal(rt.CapabilityArgs)
	   capabilityArgsValue := string(data)
	   if len(capabilityArgsValue) > 0 {
		//println("capabilityArgsValue: ", capabilityArgsValue)
		s = fmt.Sprintf("mcc: capabilityArgsValue: %v of type %T \n", capabilityArgsValue, capabilityArgsValue)
		_, _ = f.Write([]byte(s))
		if err = json.Unmarshal([]byte(capabilityArgsValue), &capabilityArgs); err != nil {
			return err
		}
		s = fmt.Sprintf("mcc: capabilityArgs: %v of type %T \n", capabilityArgs, capabilityArgs)
		_, _ = f.Write([]byte(s))
	   }
	}

	var cniArgs string
	if len(rt.Args) > 0 {
		cniArgs, _ = stringFromArgs(rt.Args)
		s = fmt.Sprintf("mcc: cniArgs: %v of type %T \n", cniArgs, cniArgs)
		_, _ = f.Write([]byte(s))
	}

	if !c.ClientgRPC {
	   return invoke.ExecPluginWithoutResult(ctx, pluginPath, newConf.Bytes, c.args("CHECK", rt), c.exec)
	} else {
	   err := gRPCsendCheck(ctx, c.Conn, string(newConf.Bytes), rt.NetNS, rt.IfName, cniArgs, capabilityArgs)
	   //err := gRPCsendCheck(ctx, c.Conn, string(net.Bytes), rt.NetNS, rt.IfName, cniArgs, capabilityArgs)
	   if err != nil {
		return err
	   }

	   return nil
	}

	return nil	
}

// CheckNetworkList executes a sequence of plugins with the CHECK command
func (c *CNIConfig) CheckNetworkList(ctx context.Context, list *NetworkConfigList, rt *RuntimeConf) error {
	// CHECK was added in CNI spec version 0.4.0 and higher
	if gtet, err := version.GreaterThanOrEqualTo(list.CNIVersion, "0.4.0"); err != nil {
		return err
	} else if !gtet {
		return fmt.Errorf("configuration version %q does not support the CHECK command", list.CNIVersion)
	}

	if list.DisableCheck {
		return nil
	}

	cachedResult, err := getCachedResult(list.Name, list.CNIVersion, rt)
	if err != nil {
		return fmt.Errorf("failed to get network %q cached result: %v", list.Name, err)
	}

	for _, net := range list.Plugins {
		if err := c.checkNetwork(ctx, list.Name, list.CNIVersion, net, cachedResult, rt); err != nil {
			return err
		}
	}

	return nil
}

func (c *CNIConfig) delNetwork(ctx context.Context, name, cniVersion string, net *NetworkConfig, prevResult types.Result, rt *RuntimeConf) error {
var f *os.File
var s string
f, _ = os.OpenFile("/tmp/check.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
defer f.Close()

if c.ClientgRPC {
   s = fmt.Sprintf("mcc: delNetwork Called as CLIENT:\n")
   _, _ = f.Write([]byte(s))
} else {
   s = fmt.Sprintf("mcc: delNetwork Called as SERVER:\n")
   _, _ = f.Write([]byte(s))
}
	c.ensureExec()
	pluginPath, err := c.exec.FindInPath(net.Network.Type, c.Path)
	if err != nil {
		return err
	}

	newConf, err := buildOneConfig(name, cniVersion, net, prevResult, rt)
	if err != nil {
		return err
	}

	capabilityArgs := CNIcapArgs{}
	if rt.CapabilityArgs != nil {
	   data, err := json.Marshal(rt.CapabilityArgs)
	   capabilityArgsValue := string(data)
	   if len(capabilityArgsValue) > 0 {
		//println("capabilityArgsValue: ", capabilityArgsValue)
		s = fmt.Sprintf("mcc: capabilityArgsValue: %v of type %T \n", capabilityArgsValue, capabilityArgsValue)
		_, _ = f.Write([]byte(s))
		if err = json.Unmarshal([]byte(capabilityArgsValue), &capabilityArgs); err != nil {
			return err
		}
		s = fmt.Sprintf("mcc: capabilityArgs: %v of type %T \n", capabilityArgs, capabilityArgs)
		_, _ = f.Write([]byte(s))
	   }
	}

	var cniArgs string
	if len(rt.Args) > 0 {
		cniArgs, _ = stringFromArgs(rt.Args)
		s = fmt.Sprintf("mcc: cniArgs: %v of type %T \n", cniArgs, cniArgs)
		_, _ = f.Write([]byte(s))
	}

	if !c.ClientgRPC {
	   return invoke.ExecPluginWithoutResult(ctx, pluginPath, newConf.Bytes, c.args("DEL", rt), c.exec)
	} else {
	   err := gRPCsendDel(ctx, c.Conn, string(newConf.Bytes), rt.NetNS, rt.IfName, cniArgs, capabilityArgs)
	   //err := gRPCsendDel(ctx, c.Conn, string(net.Bytes), rt.NetNS, rt.IfName, cniArgs, capabilityArgs)
	   if err != nil {
		return err
	   }

	   return nil
	}

	return nil
}

// DelNetworkList executes a sequence of plugins with the DEL command
func (c *CNIConfig) DelNetworkList(ctx context.Context, list *NetworkConfigList, rt *RuntimeConf) error {

	var cachedResult types.Result

	// Cached result on DEL was added in CNI spec version 0.4.0 and higher
	if gtet, err := version.GreaterThanOrEqualTo(list.CNIVersion, "0.4.0"); err != nil {
		return err
	} else if gtet {
		cachedResult, err = getCachedResult(list.Name, list.CNIVersion, rt)
		if err != nil {
			return fmt.Errorf("failed to get network %q cached result: %v", list.Name, err)
		}
	}

	for i := len(list.Plugins) - 1; i >= 0; i-- {
		net := list.Plugins[i]
		if err := c.delNetwork(ctx, list.Name, list.CNIVersion, net, cachedResult, rt); err != nil {
			return err
		}
	}
	_ = delCachedResult(list.Name, rt)

	return nil
}

// AddNetwork executes the plugin with the ADD command
func (c *CNIConfig) AddNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) (types.Result, error) {
var f *os.File
var s string
f, _ = os.OpenFile("/tmp/check.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
defer f.Close()
s = fmt.Sprintf("mcc: AddNetwork Called net.Network.Name %v \n", net.Network.Name)
_, _ = f.Write([]byte(s))
s = fmt.Sprintf("mcc: AddNetwork Called net %v \n", string(net.Bytes))
_, _ = f.Write([]byte(s))
s = fmt.Sprintf("mcc: AddNetwork Called rt %v \n", rt)
_, _ = f.Write([]byte(s))
	result, err := c.addNetwork(ctx, net.Network.Name, net.Network.CNIVersion, net, nil, rt)
	if err != nil {
		return nil, err
	}

	if err = setCachedResult(result, net.Network.Name, rt); err != nil {
		return nil, fmt.Errorf("failed to set network %q cached result: %v", net.Network.Name, err)
	}

	return result, nil
}

// CheckNetwork executes the plugin with the CHECK command
func (c *CNIConfig) CheckNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) error {
	// CHECK was added in CNI spec version 0.4.0 and higher
	if gtet, err := version.GreaterThanOrEqualTo(net.Network.CNIVersion, "0.4.0"); err != nil {
		return err
	} else if !gtet {
		return fmt.Errorf("configuration version %q does not support the CHECK command", net.Network.CNIVersion)
	}

	cachedResult, err := getCachedResult(net.Network.Name, net.Network.CNIVersion, rt)
	if err != nil {
		return fmt.Errorf("failed to get network %q cached result: %v", net.Network.Name, err)
	}
	return c.checkNetwork(ctx, net.Network.Name, net.Network.CNIVersion, net, cachedResult, rt)
}

// DelNetwork executes the plugin with the DEL command
func (c *CNIConfig) DelNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) error {

	var cachedResult types.Result

	// Cached result on DEL was added in CNI spec version 0.4.0 and higher
	if gtet, err := version.GreaterThanOrEqualTo(net.Network.CNIVersion, "0.4.0"); err != nil {
		return err
	} else if gtet {
		cachedResult, err = getCachedResult(net.Network.Name, net.Network.CNIVersion, rt)
		if err != nil {
			return fmt.Errorf("failed to get network %q cached result: %v", net.Network.Name, err)
		}
	}

	if err := c.delNetwork(ctx, net.Network.Name, net.Network.CNIVersion, net, cachedResult, rt); err != nil {
		return err
	}
	_ = delCachedResult(net.Network.Name, rt)
	return nil
}

// ValidateNetworkList checks that a configuration is reasonably valid.
// - all the specified plugins exist on disk
// - every plugin supports the desired version.
//
// Returns a list of all capabilities supported by the configuration, or error
func (c *CNIConfig) ValidateNetworkList(ctx context.Context, list *NetworkConfigList) ([]string, error) {
	version := list.CNIVersion

	// holding map for seen caps (in case of duplicates)
	caps := map[string]interface{}{}

	errs := []error{}
	for _, net := range list.Plugins {
		if err := c.validatePlugin(ctx, net.Network.Type, version); err != nil {
			errs = append(errs, err)
		}
		for c, enabled := range net.Network.Capabilities {
			if !enabled {
				continue
			}
			caps[c] = struct{}{}
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("%v", errs)
	}

	// make caps list
	cc := make([]string, 0, len(caps))
	for c := range caps {
		cc = append(cc, c)
	}

	return cc, nil
}

// ValidateNetwork checks that a configuration is reasonably valid.
// It uses the same logic as ValidateNetworkList)
// Returns a list of capabilities
func (c *CNIConfig) ValidateNetwork(ctx context.Context, net *NetworkConfig) ([]string, error) {
	caps := []string{}
	for c, ok := range net.Network.Capabilities {
		if ok {
			caps = append(caps, c)
		}
	}
	if err := c.validatePlugin(ctx, net.Network.Type, net.Network.CNIVersion); err != nil {
		return nil, err
	}
	return caps, nil
}

// validatePlugin checks that an individual plugin's configuration is sane
func (c *CNIConfig) validatePlugin(ctx context.Context, pluginName, expectedVersion string) error {
	pluginPath, err := invoke.FindInPath(pluginName, c.Path)
	if err != nil {
		return err
	}

	vi, err := invoke.GetVersionInfo(ctx, pluginPath, c.exec)
	if err != nil {
		return err
	}
	for _, vers := range vi.SupportedVersions() {
		if vers == expectedVersion {
			return nil
		}
	}
	return fmt.Errorf("plugin %s does not support config version %q", pluginName, expectedVersion)
}

// GetVersionInfo reports which versions of the CNI spec are supported by
// the given plugin.
func (c *CNIConfig) GetVersionInfo(ctx context.Context, pluginType string) (version.PluginInfo, error) {
	c.ensureExec()
	pluginPath, err := c.exec.FindInPath(pluginType, c.Path)
	if err != nil {
		return nil, err
	}

	return invoke.GetVersionInfo(ctx, pluginPath, c.exec)
}

// =====
func (c *CNIConfig) args(action string, rt *RuntimeConf) *invoke.Args {
	return &invoke.Args{
		Command:     action,
		ContainerID: rt.ContainerID,
		NetNS:       rt.NetNS,
		PluginArgs:  rt.Args,
		IfName:      rt.IfName,
		Path:        strings.Join(c.Path, string(os.PathListSeparator)),
	}
}

// Authentication holds the login/password
type Authentication struct {
	Login    string
	Password string
}

// GetRequestMetadata gets the current request metadata
func (a *Authentication) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{
		"login":    a.Login,
		"password": a.Password,
	}, nil
}

// RequireTransportSecurity indicates whether the credentials requires transport security
func (a *Authentication) RequireTransportSecurity() bool {
	return true
}

func CNIgRPCtcp() (*grpc.ClientConn, error) {
	var conn *grpc.ClientConn

	// Initiate a connection with the server
	//conn, err = grpc.Dial("localhost:7777", grpc.WithTransportCredentials(creds), grpc.WithPerRPCCredentials(&auth))
	conn, err := grpc.Dial("localhost:7777", grpc.WithInsecure())
	if err != nil {
		log.Fatalf("did not connect: %s", err)
		return nil, err
	}

	return conn, nil
}

func CNIgRPCunix() (*grpc.ClientConn, error) {

	var conn *grpc.ClientConn

	// Initiate a connection with the server
	//conn, err = grpc.Dial("unix:///tmp/grpc.sock", grpc.WithTransportCredentials(creds), grpc.WithPerRPCCredentials(&auth))
	conn, err := grpc.Dial("unix:///tmp/grpc.sock", grpc.WithInsecure())
	if err != nil {
		log.Fatalf("did not connect: %s", err)
		return nil, err
	}

	return conn, nil
}

func gRPCsendAdd(ctx context.Context, conn *grpc.ClientConn, conf string, netns string, ifName string, args string, capArgs CNIcapArgs) (error, string) {
var f *os.File
var s string
f, _ = os.OpenFile("/tmp/check.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
defer f.Close()
s = fmt.Sprintf("mcc: gRPCsendAdd Called\n")
_, _ = f.Write([]byte(s))
f.Sync()

	cni := NewCNIserverClient(conn)

	cniAddMsg := CNIaddMsg{
		Conf:    conf,
		NetNS:   netns,
		IfName:  ifName,
		CniArgs: args,
		CapArgs: &capArgs,
	}

	s = fmt.Sprintf("mcc: Send message Conf file: %s \n", cniAddMsg.Conf)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message ContainerID: %s \n", cniAddMsg.ContainerID)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message NetNS: %s \n", cniAddMsg.NetNS)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message IfName: %s \n", cniAddMsg.IfName)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message CniArgs: %s \n", cniAddMsg.CniArgs)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message CniCapArgs: %s \n", cniAddMsg.CapArgs)
	_, _ = f.Write([]byte(s))
	f.Sync()

	resultAdd, err := cni.CNIadd(ctx, &cniAddMsg)
	if err != nil {
		log.Fatalf("error when calling CNIadd: %s", err)
		return err, ""
	}
	s = fmt.Sprintf("mcc: Response from TCP server: %s (string)\n", resultAdd.StdOut)
	_, _ = f.Write([]byte(s))
	f.Sync()

	return nil, resultAdd.StdOut
}

func gRPCsendCheck(ctx context.Context, conn *grpc.ClientConn, conf string, netns string, ifName string, args string, capArgs CNIcapArgs) error {
var f *os.File
var s string
f, _ = os.OpenFile("/tmp/check.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
defer f.Close()
s = fmt.Sprintf("mcc: gRPCsendCheck Called\n")
_, _ = f.Write([]byte(s))
f.Sync()

	cni := NewCNIserverClient(conn)

	cniCheckMsg := CNIcheckMsg{
		Conf:    conf,
		NetNS:   netns,
		IfName:  ifName,
		CniArgs: args,
		CapArgs: &capArgs,
	}

	s = fmt.Sprintf("mcc: Send message Conf file: %s \n", cniCheckMsg.Conf)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message ContainerID: %s \n", cniCheckMsg.ContainerID)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message NetNS: %s \n", cniCheckMsg.NetNS)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message IfName: %s \n", cniCheckMsg.IfName)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message CniArgs: %s \n", cniCheckMsg.CniArgs)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message CniCapArgs: %s \n", cniCheckMsg.CapArgs)
	_, _ = f.Write([]byte(s))
	f.Sync()

	resultCheck, err := cni.CNIcheck(ctx, &cniCheckMsg)
	if err != nil {
		log.Fatalf("error when calling CNIcheck: %s", err)
		return err
	}
	s = fmt.Sprintf("mcc: Response from TCP server: %s (string)\n", resultCheck.Error)
	_, _ = f.Write([]byte(s))
	f.Sync()

	return nil
}

func gRPCsendDel(ctx context.Context, conn *grpc.ClientConn, conf string, netns string, ifName string, args string, capArgs CNIcapArgs) error {
var f *os.File
var s string
f, _ = os.OpenFile("/tmp/check.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
defer f.Close()
s = fmt.Sprintf("mcc: gRPCsendDel Called\n")
_, _ = f.Write([]byte(s))
f.Sync()

	cni := NewCNIserverClient(conn)

	cniMsg := CNIdelMsg{
		Conf:    conf,
		NetNS:   netns,
		IfName:  ifName,
		CniArgs: args,
		CapArgs: &capArgs,
	}

	s = fmt.Sprintf("mcc: Send message Conf file: %s \n", cniMsg.Conf)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message ContainerID: %s \n", cniMsg.ContainerID)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message NetNS: %s \n", cniMsg.NetNS)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message IfName: %s \n", cniMsg.IfName)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message CniArgs: %s \n", cniMsg.CniArgs)
	_, _ = f.Write([]byte(s))
	s = fmt.Sprintf("mcc:      message CniCapArgs: %s \n", cniMsg.CapArgs)
	_, _ = f.Write([]byte(s))
	f.Sync()

	resultDel, err := cni.CNIdel(ctx, &cniMsg)
	if err != nil {
		log.Fatalf("error when calling CNIdel: %s", err)
		return err
	}
	s = fmt.Sprintf("mcc: Response from TCP server: %s (string)\n", resultDel.Error)
	_, _ = f.Write([]byte(s))
	f.Sync()

	return nil
}

func StartGRPCunixServer(address string) error {
	// create a listener on unix socket
	syscall.Unlink(unixSocketPath)
	lis, err := net.Listen("unix", unixSocketPath)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	// create a CNI server instance
	cni := CNIServer{}

	// create a gRPC server object
	//grpcCNIServer := grpc.NewServer(opts...)
	grpcCNIServer := grpc.NewServer()

	// attach the CNI service to the server
	RegisterCNIserverServer(grpcCNIServer, &cni)

	// start the server
	log.Printf("starting CNI unix socket gRPC server on %s", unixSocketPath)
	if err := grpcCNIServer.Serve(lis); err != nil {
		return fmt.Errorf("failed to serve: %s", err)
	}

	return nil
}

func StartGRPCtcpServer(address string) error {
	// create a listener on TCP port
	lis, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	// create a CNI server instance
	cni := CNIServer{}

	// create a gRPC server object
	//grpcCNIServer := grpc.NewServer(opts...)
	grpcCNIServer := grpc.NewServer()

	// attach the CNI service to the server
	RegisterCNIserverServer(grpcCNIServer, &cni)

	// start the server
	log.Printf("starting CNI HTTP/2 gRPC server on %s", address)
	if err := grpcCNIServer.Serve(lis); err != nil {
		return fmt.Errorf("failed to serve: %s", err)
	}

	return nil
}
