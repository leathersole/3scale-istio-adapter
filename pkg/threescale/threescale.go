// nolint:lll
// Generates the Threescale adapter's resource yaml. It contains the adapter's configuration, name,
// supported template names (metric in this case), and whether it is session or no-session based.

// nolint: lll
//go:generate $GOPATH/src/istio.io/istio/bin/mixer_codegen.sh -a mixer/adapter/3scale-istio-adapter/config/config.proto -x "-s=false -n threescale -t threescale-authorization"
package threescale

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/3scale/3scale-go-client/threescale"
	"github.com/3scale/3scale-go-client/threescale/api"
	"github.com/3scale/3scale-istio-adapter/config"
	"github.com/3scale/3scale-istio-adapter/pkg/threescale/connectors/backend"
	prometheus "github.com/3scale/3scale-istio-adapter/pkg/threescale/metrics"
	sysC "github.com/3scale/3scale-porta-go-client/client"
	"github.com/gogo/googleapis/google/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"istio.io/api/mixer/adapter/model/v1beta1"
	"istio.io/istio/mixer/pkg/status"
	"istio.io/istio/mixer/template/authorization"
	"istio.io/istio/pkg/log"
)

// Implement required interface
var _ authorization.HandleAuthorizationServiceServer = &Threescale{}

const (
	pathErr            = "missing request path"
	unauthenticatedErr = "no auth credentials provided or provided in invalid location"

	// oauthTypeIdentifier refers to the name by which 3scale config described oauth OpenID connect authentication pattern
	openIDTypeIdentifier = "oauth"

	// consts reflect key values in instance config
	AppIDAttributeKey  = "app_id"
	AppKeyAttributeKey = "app_key"
	OIDCAttributeKey   = "client_id"
)

// HandleAuthorization takes care of the authorization request from mixer
func (s *Threescale) HandleAuthorization(ctx context.Context, r *authorization.HandleAuthorizationRequest) (*v1beta1.CheckResult, error) {
	var result v1beta1.CheckResult
	var systemClient sysC.ThreeScaleClient
	var proxyConfElement sysC.ProxyConfigElement
	var backendClient threescale.Client
	var backendURL string
	var err error

	log.Debugf("Got instance %+v", r.Instance)

	cfg, err := s.parseConfigParams(r)
	if err != nil {
		result.Status, err = rpcStatusErrorHandler("", status.WithInternal, err)
		goto out
	}

	// Support receiving service_id as both hardcoded value in handler and at request time
	if cfg.ServiceId == "" {
		if r.Instance.Action.Service == "" {
			result.Status, err = rpcStatusErrorHandler("error. No Service ID provided", status.WithInvalidArgument, err)
			goto out
		}
		cfg.ServiceId = r.Instance.Action.Service
	}

	systemClient, err = s.conf.clientBuilder.BuildSystemClient(cfg.SystemUrl)
	if err != nil {
		result.Status, err = rpcStatusErrorHandler("error building HTTP client for 3scale system", status.WithInvalidArgument, err)
		goto out
	}

	proxyConfElement, err = s.extractProxyConf(cfg, &systemClient)
	if err != nil {
		result.Status, err = rpcStatusErrorHandler("currently unable to fetch required data from 3scale system", status.WithUnavailable, err)
		goto out
	}

	backendURL = cfg.BackendUrl
	if backendURL == "" {
		backendURL = proxyConfElement.ProxyConfig.Content.Proxy.Backend.Endpoint
	}

	backendClient, err = s.conf.clientBuilder.BuildBackendClient(backendURL)
	if err != nil {
		result.Status, err = rpcStatusErrorHandler("error creating 3scale backend client", status.WithInvalidArgument, err)
		goto out
	}

	result.Status = s.isAuthorized(cfg.ServiceId, *r.Instance, proxyConfElement.ProxyConfig, backendClient)

	// Caching at Mixer/Envoy layer needs to be disabled currently since we would miss reporting
	// cached requests. We can determine caching values going forward by splitting the check
	// and report functionality and using cache values obtained from 3scale extension api

	// Setting a negative value will invalidate the cache - it seems from integration test
	// and manual testing that zero values for a successful check set a large default value
	result.ValidDuration = 0 * time.Second
	result.ValidUseCount = -1
	goto out
out:
	return &result, err
}

// parseConfigParams - parses the configuration passed to the adapter from mixer
// Where an error occurs during parsing, error is formatted and logged and nil value returned for config
func (s *Threescale) parseConfigParams(r *authorization.HandleAuthorizationRequest) (*config.Params, error) {
	if r.AdapterConfig == nil {
		err := errors.New("internal error - adapter config is not available")
		return nil, err
	}

	cfg := &config.Params{}
	if err := cfg.Unmarshal(r.AdapterConfig.Value); err != nil {
		unmarshalErr := errors.New("internal error - unable to unmarshal adapter config")
		return nil, unmarshalErr
	}
	return cfg, nil
}

// extractProxyConf - fetches the latest system proxy configuration or returns an error if unavailable
// If system cache is enabled, config will be fetched from the cache
func (s *Threescale) extractProxyConf(cfg *config.Params, c *sysC.ThreeScaleClient) (sysC.ProxyConfigElement, error) {
	var pce sysC.ProxyConfigElement
	var proxyConfErr error

	if s.conf.systemCache != nil {
		pce, proxyConfErr = s.conf.systemCache.Get(cfg, c)
	} else {
		pce, proxyConfErr = GetFromRemote(cfg, c, s.reportMetrics)
	}
	return pce, proxyConfErr
}

// isAuthorized - is responsible for parsing the incoming request and determining if it is valid, building out the request to be sent
// to 3scale and parsing the response. Returns code 0 if authorization is successful based on
// grpc return codes https://github.com/grpc/grpc-go/blob/master/codes/codes.go
func (s *Threescale) isAuthorized(svcID string, request authorization.InstanceMsg, proxyConf sysC.ProxyConfig, client threescale.Client) rpc.Status {
	var (
		// Application ID/OpenID Connect authentication pattern - App Key is optional when using this authn
		appID, appKey string

		// Application Key auth pattern
		userKey string
	)

	if request.Action.Path == "" {
		return status.WithInvalidArgument(pathErr)
	}

	if request.Subject != nil {
		var appIdentifierKey string

		if proxyConf.Content.BackendVersion == openIDTypeIdentifier {
			// OIDC integration configured so force app identifier to come from jwt claims
			appIdentifierKey = OIDCAttributeKey
		} else {
			appIdentifierKey = AppIDAttributeKey
		}

		appID = request.Subject.Properties[appIdentifierKey].GetStringValue()
		appKey = request.Subject.Properties[AppKeyAttributeKey].GetStringValue()

		userKey = request.Subject.User
	}

	if appID == "" && userKey == "" {
		return status.WithPermissionDenied(unauthenticatedErr)
	}

	metrics := generateMetrics(request.Action.Path, request.Action.Method, proxyConf)
	if len(metrics) == 0 {
		return status.WithPermissionDenied(fmt.Sprintf("no matching mapping rule for request with method %s and path %s",
			request.Action.Method, request.Action.Path))
	}

	authRepRequest := backend.Request{
		Auth: api.ClientAuth{
			Type:  api.AuthType(proxyConf.Content.BackendAuthenticationType),
			Value: proxyConf.Content.BackendAuthenticationValue,
		},
		ServiceID: svcID,
		Transaction: api.Transaction{
			Metrics: metrics,
			Params: api.Params{
				AppID:   appID,
				AppKey:  appKey,
				UserKey: userKey,
			},
		},
	}

	return s.apiRespConverter(s.conf.backend.AuthRep(authRepRequest, client))
}

func (s *Threescale) apiRespConverter(resp backend.Response, e error) rpc.Status {
	if e != nil {
		status, _ := rpcStatusErrorHandler("error calling 3scale backend", status.WithUnknown, e)
		return status

	}
	if !resp.Success {
		return status.WithPermissionDenied(resp.Reason)
	}

	return status.OK
}

// reportMetrics - report metrics for 3scale adapter to Prometheus. Function is safe to call if
// a reporter has not been configured
func (s *Threescale) reportMetrics(svcID string, l prometheus.LatencyReport, sr prometheus.StatusReport) {
	if s.conf != nil {
		s.conf.metricsReporter.ReportMetrics(svcID, l, sr)
	}
}

func generateMetrics(path string, method string, conf sysC.ProxyConfig) api.Metrics {
	metrics := make(api.Metrics)

	for _, pr := range conf.Content.Proxy.ProxyRules {
		if match, err := regexp.MatchString(pr.Pattern, path); err == nil {
			if match && strings.ToUpper(pr.HTTPMethod) == strings.ToUpper(method) {
				metrics.Add(pr.MetricSystemName, int(pr.Delta))
			}
		}
	}
	return metrics
}

func parseURL(url *url.URL) (string, string, int) {
	scheme := url.Scheme
	if scheme == "" {
		scheme = "https"
	}

	host, port, _ := net.SplitHostPort(url.Host)
	if port == "" {
		if scheme == "http" {
			port = "80"
		} else if scheme == "https" {
			port = "443"
		}
	}

	if host == "" {
		host = url.Host
	}

	p, _ := strconv.Atoi(port)
	return scheme, host, p
}

// rpcStatusErrorHandler provides a uniform way to log and format error messages and status which should be
// returned to the user in cases where the authorization request is rejected.
func rpcStatusErrorHandler(userFacingErrMsg string, fn func(string) rpc.Status, err error) (rpc.Status, error) {
	if userFacingErrMsg != "" {
		var errMsg string
		if err != nil {
			errMsg = fmt.Sprintf("- %s", err.Error())
		}
		err = fmt.Errorf("%s %s", userFacingErrMsg, errMsg)
	}

	log.Error(err.Error())
	return fn(err.Error()), err
}

// Addr returns the Threescale addrs as a string
func (s *Threescale) Addr() string {
	return s.listener.Addr().String()
}

// Run starts the Threescale grpc Server
func (s *Threescale) Run(shutdown chan error) {
	shutdown <- s.server.Serve(s.listener)
}

// Close stops the Threescale grpc Server
func (s *Threescale) Close() error {
	if s.server != nil {
		s.server.GracefulStop()
	}

	if s.listener != nil {
		_ = s.listener.Close()
	}

	return nil
}

// NewThreescale returns a Server interface
func NewThreescale(addr string, conf *AdapterConfig) (Server, error) {

	listener, err := net.Listen("tcp", fmt.Sprintf(":%s", addr))
	if err != nil {
		return nil, err
	}

	s := &Threescale{
		listener: listener,
		conf:     conf,
	}

	if conf != nil {
		if conf.metricsReporter != nil {
			conf.metricsReporter.Serve()
		}
	}

	log.Infof("Threescale Istio Adapter is listening on \"%v\"\n", s.Addr())

	s.server = grpc.NewServer(grpc.KeepaliveParams(keepalive.ServerParameters{
		MaxConnectionAge: conf.keepAliveMaxAge,
	}))
	authorization.RegisterHandleAuthorizationServiceServer(s.server, s)
	return s, nil
}

// NewAdapterConfig - Creates configuration for Threescale adapter
func NewAdapterConfig(builder Builder, cache *ProxyConfigCache, metrics *prometheus.Reporter, b backend.Backend, grpcKeepalive time.Duration) *AdapterConfig {
	if cache != nil {
		cache.metricsReporter = metrics
	}

	var reportFn prometheus.ReportMetricsFn
	if metrics != nil {
		reportFn = metrics.ReportMetrics
	}

	if b == nil {
		b = backend.DefaultBackend{ReportFn: reportFn}
	}

	return &AdapterConfig{builder, cache, metrics, b, grpcKeepalive}
}
