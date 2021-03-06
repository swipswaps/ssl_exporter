package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"github.com/ribbybibby/ssl_exporter/config"
	"github.com/ribbybibby/ssl_exporter/prober"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace = "ssl"
)

var (
	tlsConnectSuccess = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "tls_connect_success"),
		"If the TLS connection was a success",
		nil, nil,
	)
	tlsVersion = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "tls_version_info"),
		"The TLS version used",
		[]string{"version"}, nil,
	)
	proberType = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "prober"),
		"The prober used by the exporter to connect to the target",
		[]string{"prober"}, nil,
	)
	notBefore = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "cert_not_before"),
		"NotBefore expressed as a Unix Epoch Time",
		[]string{"serial_no", "issuer_cn", "cn", "dnsnames", "ips", "emails", "ou"}, nil,
	)
	notAfter = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "cert_not_after"),
		"NotAfter expressed as a Unix Epoch Time",
		[]string{"serial_no", "issuer_cn", "cn", "dnsnames", "ips", "emails", "ou"}, nil,
	)
	verifiedNotBefore = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "verified_cert_not_before"),
		"NotBefore expressed as a Unix Epoch Time for a certificate in the list of verified chains",
		[]string{"chain_no", "serial_no", "issuer_cn", "cn", "dnsnames", "ips", "emails", "ou"}, nil,
	)
	verifiedNotAfter = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "verfied_cert_not_after"),
		"NotAfter expressed as a Unix Epoch Time for a certificate in the list of verified chains",
		[]string{"chain_no", "serial_no", "issuer_cn", "cn", "dnsnames", "ips", "emails", "ou"}, nil,
	)
)

// Exporter is the exporter type...
type Exporter struct {
	target  string
	prober  prober.ProbeFn
	timeout time.Duration
	module  config.Module
}

// Describe metrics
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- tlsConnectSuccess
	ch <- tlsVersion
	ch <- proberType
	ch <- notAfter
	ch <- notBefore
	ch <- verifiedNotAfter
	ch <- verifiedNotBefore
}

// Collect metrics
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(
		proberType, prometheus.GaugeValue, 1, e.module.Prober,
	)

	state, err := e.prober(e.target, e.module, e.timeout)
	if err != nil {
		log.Errorf("error=%s target=%s prober=%s timeout=%s", err, e.target, e.module.Prober, e.timeout)
		ch <- prometheus.MustNewConstMetric(
			tlsConnectSuccess, prometheus.GaugeValue, 0,
		)
		return
	}

	// Get the TLS version from the connection state and export it as a metric
	ch <- prometheus.MustNewConstMetric(
		tlsVersion, prometheus.GaugeValue, 1, getTLSVersion(state),
	)

	// Retrieve certificates from the connection state
	peerCertificates := state.PeerCertificates
	if len(peerCertificates) < 1 {
		log.Errorf("error=No certificates found in connection state. target=%s prober=%s", e.target, e.module.Prober)
		ch <- prometheus.MustNewConstMetric(
			tlsConnectSuccess, prometheus.GaugeValue, 0,
		)
		return
	}

	// If there are peer certificates in the connection state then consider
	// the tls connection a success
	ch <- prometheus.MustNewConstMetric(
		tlsConnectSuccess, prometheus.GaugeValue, 1,
	)

	// Remove duplicate certificates from the response
	peerCertificates = uniq(peerCertificates)

	// Loop through peer certificates and create metrics
	for _, cert := range peerCertificates {
		if !cert.NotAfter.IsZero() {
			ch <- prometheus.MustNewConstMetric(
				notAfter,
				prometheus.GaugeValue,
				float64(cert.NotAfter.UnixNano()/1e9),
				cert.SerialNumber.String(),
				cert.Issuer.CommonName,
				cert.Subject.CommonName,
				getDNSNames(cert),
				getIPAddresses(cert),
				getEmailAddresses(cert),
				getOrganizationalUnits(cert),
			)
		}

		if !cert.NotBefore.IsZero() {
			ch <- prometheus.MustNewConstMetric(
				notBefore,
				prometheus.GaugeValue,
				float64(cert.NotBefore.UnixNano()/1e9),
				cert.SerialNumber.String(),
				cert.Issuer.CommonName,
				cert.Subject.CommonName,
				getDNSNames(cert),
				getIPAddresses(cert),
				getEmailAddresses(cert),
				getOrganizationalUnits(cert),
			)
		}
	}

	// Retrieve the list of verified chains from the connection state
	verifiedChains := state.VerifiedChains

	// Sort the verified chains from the chain that is valid for longest to the chain
	// that expires the soonest
	sort.Slice(verifiedChains, func(i, j int) bool {
		iExpiry := time.Time{}
		for _, cert := range verifiedChains[i] {
			if (iExpiry.IsZero() || cert.NotAfter.Before(iExpiry)) && !cert.NotAfter.IsZero() {
				iExpiry = cert.NotAfter
			}
		}
		jExpiry := time.Time{}
		for _, cert := range verifiedChains[j] {
			if (jExpiry.IsZero() || cert.NotAfter.Before(jExpiry)) && !cert.NotAfter.IsZero() {
				jExpiry = cert.NotAfter
			}
		}

		return iExpiry.After(jExpiry)
	})

	// Loop through the verified chains creating metrics. Label the metrics
	// with the index of the chain.
	for i, chain := range verifiedChains {
		chain = uniq(chain)
		for _, cert := range chain {
			chainNo := strconv.Itoa(i)

			if !cert.NotAfter.IsZero() {
				ch <- prometheus.MustNewConstMetric(
					verifiedNotAfter,
					prometheus.GaugeValue,
					float64(cert.NotAfter.UnixNano()/1e9),
					chainNo,
					cert.SerialNumber.String(),
					cert.Issuer.CommonName,
					cert.Subject.CommonName,
					getDNSNames(cert),
					getIPAddresses(cert),
					getEmailAddresses(cert),
					getOrganizationalUnits(cert),
				)
			}

			if !cert.NotBefore.IsZero() {
				ch <- prometheus.MustNewConstMetric(
					verifiedNotBefore,
					prometheus.GaugeValue,
					float64(cert.NotBefore.UnixNano()/1e9),
					chainNo,
					cert.SerialNumber.String(),
					cert.Issuer.CommonName,
					cert.Subject.CommonName,
					getDNSNames(cert),
					getIPAddresses(cert),
					getEmailAddresses(cert),
					getOrganizationalUnits(cert),
				)
			}
		}
	}
}

func probeHandler(w http.ResponseWriter, r *http.Request, conf *config.Config) {
	moduleName := r.URL.Query().Get("module")
	if moduleName == "" {
		moduleName = "tcp"
	}
	module, ok := conf.Modules[moduleName]
	if !ok {
		http.Error(w, fmt.Sprintf("Unknown module %q", moduleName), http.StatusBadRequest)
		return
	}

	// The following timeout block was taken wholly from the blackbox exporter
	//   https://github.com/prometheus/blackbox_exporter/blob/master/main.go
	var timeoutSeconds float64
	if v := r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds"); v != "" {
		var err error
		timeoutSeconds, err = strconv.ParseFloat(v, 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to parse timeout from Prometheus header: %s", err), http.StatusInternalServerError)
			return
		}
	} else {
		timeoutSeconds = 10
	}
	if timeoutSeconds == 0 {
		timeoutSeconds = 10
	}

	timeout := time.Duration((timeoutSeconds) * 1e9)

	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "Target parameter is missing", http.StatusBadRequest)
		return
	}

	prober, ok := prober.Probers[module.Prober]
	if !ok {
		http.Error(w, fmt.Sprintf("Unknown prober %q", module.Prober), http.StatusBadRequest)
		return
	}

	exporter := &Exporter{
		target:  target,
		prober:  prober,
		timeout: timeout,
		module:  module,
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(exporter)

	// Serve
	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}

func uniq(certs []*x509.Certificate) []*x509.Certificate {
	r := []*x509.Certificate{}

	for _, c := range certs {
		if !contains(r, c) {
			r = append(r, c)
		}
	}

	return r
}

func contains(certs []*x509.Certificate, cert *x509.Certificate) bool {
	for _, c := range certs {
		if (c.SerialNumber.String() == cert.SerialNumber.String()) && (c.Issuer.CommonName == cert.Issuer.CommonName) {
			return true
		}
	}
	return false
}

func getTLSVersion(state *tls.ConnectionState) string {
	switch state.Version {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return "unknown"
	}
}

func getDNSNames(cert *x509.Certificate) string {
	if len(cert.DNSNames) > 0 {
		return "," + strings.Join(cert.DNSNames, ",") + ","
	}

	return ""
}

func getEmailAddresses(cert *x509.Certificate) string {
	if len(cert.EmailAddresses) > 0 {
		return "," + strings.Join(cert.EmailAddresses, ",") + ","
	}

	return ""
}

func getIPAddresses(cert *x509.Certificate) string {
	if len(cert.IPAddresses) > 0 {
		ips := ","
		for _, ip := range cert.IPAddresses {
			ips = ips + ip.String() + ","
		}
		return ips
	}

	return ""
}

func getOrganizationalUnits(cert *x509.Certificate) string {
	if len(cert.Subject.OrganizationalUnit) > 0 {
		return "," + strings.Join(cert.Subject.OrganizationalUnit, ",") + ","
	}

	return ""
}

func init() {
	prometheus.MustRegister(version.NewCollector(namespace + "_exporter"))
}

func main() {
	var (
		listenAddress = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9219").String()
		metricsPath   = kingpin.Flag("web.metrics-path", "Path under which to expose metrics").Default("/metrics").String()
		probePath     = kingpin.Flag("web.probe-path", "Path under which to expose the probe endpoint").Default("/probe").String()
		configFile    = kingpin.Flag("config.file", "SSL exporter configuration file").Default("").String()
		err           error
	)

	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print(namespace + "_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	conf := config.DefaultConfig
	if *configFile != "" {
		conf, err = config.LoadConfig(*configFile)
		if err != nil {
			log.Fatalln(err)
		}
	}

	log.Infoln("Starting "+namespace+"_exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc(*probePath, func(w http.ResponseWriter, r *http.Request) {
		probeHandler(w, r, conf)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>
						 <head><title>SSL Exporter</title></head>
						 <body>
						 <h1>SSL Exporter</h1>
						 <p><a href="` + *probePath + `?target=example.com:443">Probe example.com:443 for SSL cert metrics</a></p>
						 <p><a href='` + *metricsPath + `'>Metrics</a></p>
						 </body>
						 </html>`))
	})

	log.Infoln("Listening on", *listenAddress)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
