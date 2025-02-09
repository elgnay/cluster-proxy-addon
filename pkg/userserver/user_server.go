package userserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/stolostron/cluster-proxy-addon/pkg/constant"
	"github.com/stolostron/cluster-proxy-addon/pkg/utils"
	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"k8s.io/klog/v2"
	addonutils "open-cluster-management.io/addon-framework/pkg/utils"
	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

func NewUserServerCommand() *cobra.Command {
	userServer := newUserServer()

	cmd := &cobra.Command{
		Use:   "user-server",
		Short: "user-server",
		Long:  `A http proxy server, receives http requests from users and forwards to the ANP proxy-server.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return userServer.Run(cmd.Context())
		},
	}

	userServer.AddFlags(cmd)
	return cmd
}

var (
	serviceProxyRootCA *x509.CertPool
)

type userServer struct {
	// TODO: make it a controller and reuse tunnel for each cluster to improve performance.
	getTunnel       func(context.Context) (konnectivity.Tunnel, error)
	proxyServerHost string
	proxyServerPort int

	proxyCACertPath, proxyCertPath, proxyKeyPath string

	serverCert, serverKey string
	serverPort            int

	serviceProxyCACertPath string
	agentInstallNamespace  string
}

func (k *userServer) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()

	flags.StringVar(&k.proxyServerHost, "host", k.proxyServerHost, "The host of the ANP proxy-server")
	flags.IntVar(&k.proxyServerPort, "port", k.proxyServerPort, "The port of the ANP proxy-server")

	flags.StringVar(&k.proxyCACertPath, "proxy-ca-cert", k.proxyCACertPath, "The path to the CA certificate of the ANP proxy-server")
	flags.StringVar(&k.proxyCertPath, "proxy-cert", k.proxyCertPath, "The path to the certificate of the ANP proxy-server")
	flags.StringVar(&k.proxyKeyPath, "proxy-key", k.proxyKeyPath, "The path to the key of the ANP proxy-server")

	flags.StringVar(&k.serverCert, "server-cert", k.serverCert, "Secure communication with this cert")
	flags.StringVar(&k.serverKey, "server-key", k.serverKey, "Secure communication with this key")
	flags.IntVar(&k.serverPort, "server-port", k.serverPort, "handle user request using this port")

	flags.StringVar(&k.serviceProxyCACertPath, "service-proxy-ca-cert", k.serviceProxyCACertPath, "The path to the CA certificate of the service proxy server")

	flags.StringVar(&k.agentInstallNamespace, "agent-install-namespace", k.agentInstallNamespace, "The namespace of the agent install")
}

func (k *userServer) Validate() error {
	if k.serverCert == "" {
		return fmt.Errorf("The server-cert is required")
	}

	if k.serverKey == "" {
		return fmt.Errorf("The server-key is required")
	}

	if k.serverPort == 0 {
		return fmt.Errorf("The server-port is required")
	}

	if k.serviceProxyCACertPath == "" {
		return fmt.Errorf("The serviceproxy-ca-cert is required")
	}

	return nil
}

func newUserServer() *userServer {
	return &userServer{}
}

func (k *userServer) init(ctx context.Context) error {
	proxyTLSCfg, err := util.GetClientTLSConfig(k.proxyCACertPath, k.proxyCertPath, k.proxyKeyPath, k.proxyServerHost)
	if err != nil {
		return err
	}

	// prepare ca for sevice proxy server
	serviceProxyCaCert, err := ioutil.ReadFile(k.serviceProxyCACertPath)
	if err != nil {
		return err
	}
	serviceProxyRootCA = x509.NewCertPool()
	if ok := serviceProxyRootCA.AppendCertsFromPEM(serviceProxyCaCert); !ok {
		return fmt.Errorf("failed to parse service proxy ca cert")
	}

	k.getTunnel = func(tunnelCtx context.Context) (konnectivity.Tunnel, error) {
		// instantiate a gprc proxy dialer
		tunnel, err := konnectivity.CreateSingleUseGrpcTunnelWithContext(
			ctx,
			tunnelCtx,
			net.JoinHostPort(k.proxyServerHost, strconv.Itoa(k.proxyServerPort)),
			grpc.WithTransportCredentials(grpccredentials.NewTLS(proxyTLSCfg)),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time: time.Second * 5,
			}),
		)
		if err != nil {
			return nil, err
		}
		return tunnel, nil
	}
	return nil
}

func (k *userServer) ServeHTTP(wr http.ResponseWriter, req *http.Request) {
	if klog.V(4).Enabled() {
		dump, err := httputil.DumpRequest(req, true)
		if err != nil {
			http.Error(wr, err.Error(), http.StatusBadRequest)
			return
		}
		klog.V(4).Infof("request:\n%s", string(dump))
	}

	var tsc utils.TargetServiceConfig
	var err error

	if utils.IsProxyService(req.RequestURI) {
		tsc, err = utils.GetTargetServiceConfig(req.RequestURI)
		if err != nil {
			http.Error(wr, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		tsc, err = utils.GetTargetServiceConfigForKubeAPIServer(req.RequestURI)
		if err != nil {
			http.Error(wr, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// get service proxy host for current managed cluster
	targetURL, err := url.Parse(utils.GenerateServiceProxyURL(tsc.Cluster, k.agentInstallNamespace, constant.ServiceProxyName))
	if err != nil {
		http.Error(wr, err.Error(), http.StatusBadRequest)
		return
	}

	tunnel, err := k.getTunnel(req.Context())
	if err != nil {
		http.Error(wr, err.Error(), http.StatusBadRequest)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Transport = &http.Transport{
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			RootCAs:    serviceProxyRootCA,
			MinVersion: tls.VersionTLS13,
		},
		// golang http pkg automaticly upgrade http connection to http2 connection, but http2 can not upgrade to SPDY which used in "kubectl exec".
		// set ForceAttemptHTTP2 = false to prevent auto http2 upgration
		ForceAttemptHTTP2: false,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			klog.V(4).Infof("proxy dial to %s", addr)
			// TODO: may find a way to cache the proxyConn.
			return tunnel.DialContext(ctx, network, addr)
		},
	}

	proxy.ErrorHandler = func(rw http.ResponseWriter, r *http.Request, e error) {
		http.Error(rw, fmt.Sprintf("proxy to anp-proxy-server failed because %v", e), http.StatusBadGateway)
		klog.Errorf("proxy to anp-proxy-server failed because %v", e)
	}

	klog.V(4).Infof("request scheme:%s; rawQuery:%s; path:%s", req.URL.Scheme, req.URL.RawQuery, req.URL.Path)

	proxy.ServeHTTP(wr, tsc.UpdateRequest(req))
}

func (k *userServer) Run(ctx context.Context) error {
	var err error

	klog.Info("begin to run user server")

	if err = k.Validate(); err != nil {
		klog.Fatal(err)
	}

	if err = k.init(ctx); err != nil {
		klog.Fatal(err)
	}

	cc, err := addonutils.NewConfigChecker("user-server", k.proxyCACertPath, k.proxyCertPath, k.proxyKeyPath, k.serverCert, k.serverKey, k.serviceProxyCACertPath)
	if err != nil {
		klog.Fatal(err)
	}

	go func() {
		if err = utils.ServeHealthProbes(":8000", cc.Check); err != nil {
			klog.Fatal(err)
		}
	}()

	klog.Infof("start https server on %d", k.serverPort)

	s := &http.Server{
		Addr:      fmt.Sprintf(":%d", k.serverPort),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13},
		Handler:   k,
	}

	err = s.ListenAndServeTLS(k.serverCert, k.serverKey)
	if err != nil {
		klog.Fatalf("failed to start user proxy server: %v", err)
	}

	return nil
}
