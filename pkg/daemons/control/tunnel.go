package control

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/k3s-io/k3s/pkg/daemons/config"
	"github.com/k3s-io/k3s/pkg/daemons/control/proxy"
	"github.com/k3s-io/k3s/pkg/generated/clientset/versioned/scheme"
	"github.com/k3s-io/k3s/pkg/nodeconfig"
	"github.com/k3s-io/k3s/pkg/util"
	"github.com/k3s-io/k3s/pkg/version"
	"github.com/pkg/errors"
	"github.com/rancher/remotedialer"
	"github.com/sirupsen/logrus"
	"github.com/yl2chen/cidranger"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/client-go/kubernetes"
)

func loggingErrorWriter(rw http.ResponseWriter, req *http.Request, code int, err error) {
	logrus.Debugf("Tunnel server error: %d %v", code, err)
	rw.WriteHeader(code)
	rw.Write([]byte(err.Error()))
}

func setupTunnel(ctx context.Context, cfg *config.Control) (http.Handler, error) {
	tunnel := &TunnelServer{
		cidrs:  cidranger.NewPCTrieRanger(),
		config: cfg,
		server: remotedialer.New(authorizer, loggingErrorWriter),
		egress: map[string]bool{},
	}
	go tunnel.watch(ctx)
	return tunnel, nil
}

func authorizer(req *http.Request) (clientKey string, authed bool, err error) {
	user, ok := request.UserFrom(req.Context())
	if !ok {
		return "", false, nil
	}

	if strings.HasPrefix(user.GetName(), "system:node:") {
		return strings.TrimPrefix(user.GetName(), "system:node:"), true, nil
	}

	return "", false, nil
}

// explicit interface check
var _ http.Handler = &TunnelServer{}

type TunnelServer struct {
	sync.Mutex
	cidrs  cidranger.Ranger
	client kubernetes.Interface
	config *config.Control
	server *remotedialer.Server
	egress map[string]bool
}

// explicit interface check
var _ cidranger.RangerEntry = &tunnelEntry{}

type tunnelEntry struct {
	cidr     net.IPNet
	nodeName string
	node     bool
}

func (n *tunnelEntry) Network() net.IPNet {
	return n.cidr
}

// ServeHTTP handles either CONNECT requests, or websocket requests to the remotedialer server
func (t *TunnelServer) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	logrus.Debugf("Tunnel server handing %s %s request for %s from %s", req.Proto, req.Method, req.URL, req.RemoteAddr)
	if req.Method == http.MethodConnect {
		t.serveConnect(resp, req)
	} else {
		t.server.ServeHTTP(resp, req)
	}
}

// watch waits for the runtime core to become available,
// and registers OnChange handlers to observe changes to Nodes (and Endpoints if necessary).
func (t *TunnelServer) watch(ctx context.Context) {
	logrus.Infof("Tunnel server egress proxy mode: %s", t.config.EgressSelectorMode)

	if t.config.EgressSelectorMode == config.EgressSelectorModeDisabled {
		return
	}

	for {
		if t.config.Runtime.Core != nil {
			t.config.Runtime.Core.Core().V1().Node().OnChange(ctx, version.Program+"-tunnel-server", t.onChangeNode)
			switch t.config.EgressSelectorMode {
			case config.EgressSelectorModeCluster, config.EgressSelectorModePod:
				t.config.Runtime.Core.Core().V1().Pod().OnChange(ctx, version.Program+"-tunnel-server", t.onChangePod)
			}
			return
		}
		logrus.Infof("Tunnel server egress proxy waiting for runtime core to become available")
		time.Sleep(5 * time.Second)
	}
}

// onChangeNode updates the node address mappings by observing changes to nodes.
func (t *TunnelServer) onChangeNode(nodeName string, node *v1.Node) (*v1.Node, error) {
	if node != nil {
		t.Lock()
		defer t.Unlock()
		_, t.egress[nodeName] = node.Labels[nodeconfig.ClusterEgressLabel]
		// Add all node IP addresses
		for _, addr := range node.Status.Addresses {
			if addr.Type == v1.NodeInternalIP || addr.Type == v1.NodeExternalIP {
				if n, err := util.IPStringToIPNet(addr.Address); err == nil {
					if node.DeletionTimestamp != nil {
						logrus.Debugf("Tunnel server egress proxy removing Node %s IP %v", nodeName, n)
						t.cidrs.Remove(*n)
					} else {
						logrus.Debugf("Tunnel server egress proxy updating Node %s IP %v", nodeName, n)
						t.cidrs.Insert(&tunnelEntry{cidr: *n, nodeName: nodeName, node: true})
					}
				}
			}
		}
	}
	return node, nil
}

// onChangePod updates the pod address mappings by observing changes to pods.
func (t *TunnelServer) onChangePod(podName string, pod *v1.Pod) (*v1.Pod, error) {
	if pod != nil {
		t.Lock()
		defer t.Unlock()
		// Add all pod IPs, unless the pod uses host network
		if !pod.Spec.HostNetwork {
			nodeName := pod.Spec.NodeName
			for _, ip := range pod.Status.PodIPs {
				if cidr, err := util.IPStringToIPNet(ip.IP); err == nil {
					if pod.DeletionTimestamp != nil {
						logrus.Debugf("Tunnel server egress proxy removing Node %s Pod IP %v", nodeName, cidr)
						t.cidrs.Remove(*cidr)
					} else {
						logrus.Debugf("Tunnel server egress proxy updating Node %s Pod IP %s", nodeName, cidr)
						t.cidrs.Insert(&tunnelEntry{cidr: *cidr, nodeName: nodeName})
					}
				}
			}
		}
	}
	return pod, nil

}

// serveConnect attempts to handle the HTTP CONNECT request by dialing
// a connection, either locally or via the remotedialer tunnel.
func (t *TunnelServer) serveConnect(resp http.ResponseWriter, req *http.Request) {
	bconn, err := t.dialBackend(req.Host)
	if err != nil {
		responsewriters.ErrorNegotiated(
			apierrors.NewInternalError(errors.Wrap(err, "no tunnels available")),
			scheme.Codecs.WithoutConversion(), schema.GroupVersion{}, resp, req,
		)
		return
	}

	hijacker, ok := resp.(http.Hijacker)
	if !ok {
		responsewriters.ErrorNegotiated(
			apierrors.NewInternalError(errors.New("hijacking not supported")),
			scheme.Codecs.WithoutConversion(), schema.GroupVersion{}, resp, req,
		)
		return
	}
	resp.WriteHeader(http.StatusOK)

	rconn, _, err := hijacker.Hijack()
	if err != nil {
		responsewriters.ErrorNegotiated(
			apierrors.NewInternalError(err),
			scheme.Codecs.WithoutConversion(), schema.GroupVersion{}, resp, req,
		)
		return
	}

	proxy.Proxy(rconn, bconn)
}

// dialBackend determines where to route the connection request to, and returns
// a dialed connection if possible. Note that in the case of a remotedialer
// tunnel connection, the agent may return an error if the agent's authorizer
// denies the connection, or if there is some other error in actually dialing
// the requested endpoint.
func (t *TunnelServer) dialBackend(addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	loopback := t.config.Loopback()

	var nodeName string
	var toKubelet, useTunnel bool
	if ip := net.ParseIP(host); ip != nil {
		// Destination is an IP address, which could be either a pod, or node by IP.
		// We can only use the tunnel for egress to pods if the agent supports it.
		if nets, err := t.cidrs.ContainingNetworks(ip); err == nil && len(nets) > 0 {
			if n, ok := nets[0].(*tunnelEntry); ok {
				nodeName = n.nodeName
				if n.node && config.KubeletReservedPorts[port] {
					toKubelet = true
					useTunnel = true
				} else {
					useTunnel = t.egress[nodeName]
				}
			} else {
				logrus.Debugf("Tunnel server egress proxy CIDR lookup returned unknown type for address %s", ip)
			}
		}
	} else {
		// Destination is a node by name, it is safe to use the tunnel.
		nodeName = host
		toKubelet = true
		useTunnel = true
	}

	// Always dial kubelet via the loopback address.
	if toKubelet {
		addr = fmt.Sprintf("%s:%s", loopback, port)
	}

	// If connecting to something hosted by the local node, don't tunnel
	if nodeName == t.config.ServerNodeName {
		useTunnel = false
	}

	if t.server.HasSession(nodeName) {
		if useTunnel {
			// Have a session and it is safe to use for this destination, do so.
			logrus.Debugf("Tunnel server egress proxy dialing %s via session to %s", addr, nodeName)
			return t.server.Dial(nodeName, 15*time.Second, "tcp", addr)
		}
		// Have a session but the agent doesn't support tunneling to this destination or
		// the destination is local; fall back to direct connection.
		logrus.Debugf("Tunnel server egress proxy dialing %s directly", addr)
		return net.Dial("tcp", addr)
	}

	// don't provide a proxy connection for anything else
	logrus.Debugf("Tunnel server egress proxy rejecting connection to %s", addr)
	return nil, fmt.Errorf("no sessions available for host %s", host)
}
