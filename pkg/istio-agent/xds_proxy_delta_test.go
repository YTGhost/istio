// Copyright Istio Authors
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

package istioagent

import (
	"errors"
	"os"
	"path"
	"testing"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	wasm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	wasmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/wasm/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/util/protoconv"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/istio/pilot/test/xds"
	"istio.io/istio/pilot/test/xdstest"
	"istio.io/istio/pkg/test/env"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/retry"
)

// TestXdsLeak is a regression test for https://github.com/istio/istio/issues/34097
func TestDeltaXdsLeak(t *testing.T) {
	proxy := setupXdsProxyWithDownstreamOptions(t, []grpc.ServerOption{grpc.StreamInterceptor(xdstest.SlowServerInterceptor(time.Second, time.Second))})
	f := xdstest.NewMockServer(t)
	setDialOptions(proxy, f.Listener)
	proxy.dialOptions = append(proxy.dialOptions, grpc.WithStreamInterceptor(xdstest.SlowClientInterceptor(0, time.Second*10)))
	conn := setupDownstreamConnection(t, proxy)
	downstream := deltaStream(t, conn)
	sendDeltaDownstreamWithoutResponse(t, downstream)
	for i := 0; i < 15; i++ {
		// Send a bunch of responses from Istiod. These should not block, even though there are more sends than responseChan can hold
		f.SendDeltaResponse(&discovery.DeltaDiscoveryResponse{TypeUrl: v3.ClusterType})
	}
	// Exit test, closing the connections. We should not have any goroutine leaks (checked by leak.CheckMain)
}

// sendDownstreamWithoutResponse sends a response without waiting for a response
func sendDeltaDownstreamWithoutResponse(t *testing.T, downstream discovery.AggregatedDiscoveryService_DeltaAggregatedResourcesClient) {
	t.Helper()
	err := downstream.Send(&discovery.DeltaDiscoveryRequest{
		TypeUrl: v3.ClusterType,
		Node: &core.Node{
			Id: "sidecar~0.0.0.0~debug~cluster.local",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Validates basic xds proxy flow by proxying one CDS requests end to end.
func TestDeltaXdsProxyBasicFlow(t *testing.T) {
	proxy := setupXdsProxy(t)
	f := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{})
	setDialOptions(proxy, f.BufListener)
	conn := setupDownstreamConnection(t, proxy)
	downstream := deltaStream(t, conn)
	sendDeltaDownstreamWithNode(t, downstream, model.NodeMetadata{
		Namespace:   "default",
		InstanceIPs: []string{"1.1.1.1"},
	})
}

func deltaStream(t *testing.T, conn *grpc.ClientConn) discovery.AggregatedDiscoveryService_DeltaAggregatedResourcesClient {
	t.Helper()
	adsClient := discovery.NewAggregatedDiscoveryServiceClient(conn)
	downstream, err := adsClient.DeltaAggregatedResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return downstream
}

func sendDeltaDownstreamWithNode(t *testing.T, downstream discovery.AggregatedDiscoveryService_DeltaAggregatedResourcesClient, meta model.NodeMetadata) {
	t.Helper()
	node := &core.Node{
		Id:       "sidecar~1.1.1.1~debug~cluster.local",
		Metadata: meta.ToStruct(),
	}
	err := downstream.Send(&discovery.DeltaDiscoveryRequest{TypeUrl: v3.ClusterType, Node: node})
	if err != nil {
		t.Fatal(err)
	}
	res, err := downstream.Recv()
	if err != nil {
		t.Fatal(err)
	}

	if res == nil || res.TypeUrl != v3.ClusterType {
		t.Fatalf("Expected to get cluster response but got %v", res)
	}
	err = downstream.Send(&discovery.DeltaDiscoveryRequest{TypeUrl: v3.ListenerType, Node: node})
	if err != nil {
		t.Fatal(err)
	}
	res, err = downstream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.TypeUrl != v3.ListenerType {
		t.Fatalf("Expected to get listener response but got %v", res)
	}
}

func TestDeltaECDSWasmConversion(t *testing.T) {
	node := model.NodeMetadata{
		Namespace:   "default",
		InstanceIPs: []string{"1.1.1.1"},
		ClusterID:   "Kubernetes",
	}
	proxy := setupXdsProxy(t)
	// Reset wasm cache to a fake ACK cache.
	proxy.wasmCache.Cleanup()
	proxy.wasmCache = &fakeAckCache{}

	// Initialize discovery server with an ECDS resource.
	ef, err := os.ReadFile(path.Join(env.IstioSrc, "pilot/pkg/xds/testdata/ecds.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f := xds.NewFakeDiscoveryServer(t, xds.FakeOptions{
		ConfigString: string(ef),
	})
	setDialOptions(proxy, f.BufListener)
	conn := setupDownstreamConnection(t, proxy)
	downstream := deltaStream(t, conn)

	// Send first request, wasm module async fetch should work fine and
	// downstream should receive a local file based wasm extension config.
	err = downstream.Send(&discovery.DeltaDiscoveryRequest{
		TypeUrl:                v3.ExtensionConfigurationType,
		ResourceNamesSubscribe: []string{"extension-config"},
		Node: &core.Node{
			Id:       "sidecar~1.1.1.1~debug~cluster.local",
			Metadata: node.ToStruct(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify that the request received has been rewritten to local file.
	gotResp, err := downstream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotResp.Resources) != 1 {
		t.Errorf("xds proxy ecds wasm conversion number of received resource got %v want 1", len(gotResp.Resources))
	}
	gotEcdsConfig := &core.TypedExtensionConfig{}
	if err := gotResp.Resources[0].Resource.UnmarshalTo(gotEcdsConfig); err != nil {
		t.Fatalf("wasm config conversion output %v failed to unmarshal", gotResp.Resources[0])
	}
	assert.Equal(t, gotResp.Resources[0].Name, "extension-config")
	wasm := &wasm.Wasm{
		Config: &wasmv3.PluginConfig{
			Vm: &wasmv3.PluginConfig_VmConfig{
				VmConfig: &wasmv3.VmConfig{
					Code: &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Local{
						Local: &core.DataSource{
							Specifier: &core.DataSource_Filename{
								Filename: "test",
							},
						},
					}},
				},
			},
		},
	}
	wantEcdsConfig := &core.TypedExtensionConfig{
		Name:        "extension-config",
		TypedConfig: protoconv.MessageToAny(wasm),
	}

	if !proto.Equal(gotEcdsConfig, wantEcdsConfig) {
		t.Errorf("xds proxy wasm config conversion got %v want %v", gotEcdsConfig, wantEcdsConfig)
	}
	n1 := proxy.ecdsLastNonce

	// reset wasm cache to a NACK cache, and recreate xds server as well to simulate a version bump
	proxy.wasmCache = &fakeNackCache{}
	f = xds.NewFakeDiscoveryServer(t, xds.FakeOptions{
		ConfigString: string(ef),
	})
	setDialOptions(proxy, f.BufListener)
	conn = setupDownstreamConnection(t, proxy)
	downstream = deltaStream(t, conn)

	// send request again, this time a nack should be returned from the xds proxy due to NACK wasm cache.
	err = downstream.Send(&discovery.DeltaDiscoveryRequest{
		TypeUrl:                v3.ExtensionConfigurationType,
		ResourceNamesSubscribe: []string{"extension-config"},
		Node: &core.Node{
			Id:       "sidecar~1.1.1.1~debug~cluster.local",
			Metadata: node.ToStruct(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait until nonce was updated, which represents an ACK/NACK has been received.
	retry.UntilSuccessOrFail(t, func() error {
		if proxy.ecdsLastNonce.Load() == n1.Load() {
			return errors.New("last process nonce has not been updated. no ecds ack/nack is received yet")
		}
		return nil
	}, retry.Timeout(time.Second), retry.Delay(time.Millisecond))
}
