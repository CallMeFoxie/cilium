// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package kvstore

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	client "go.etcd.io/etcd/client/v3"
	clientyaml "go.etcd.io/etcd/client/v3/yaml"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"sigs.k8s.io/yaml"

	"github.com/cilium/cilium/pkg/time"
)

var etcdVersionRegexp = regexp.MustCompile(`"etcdserver":"(?P<version>.*?)"`)

// EtcdDbgDialer enables to override the LookupIP and DialContext functions,
// e.g., to support service name to IP address resolution when CoreDNS is not
// the configured DNS server --- for pods running in the host network namespace.
type EtcdDbgDialer interface {
	LookupIP(ctx context.Context, hostname string) ([]net.IP, error)
	DialContext(ctx context.Context, addr string) (net.Conn, error)
}

// DefaultEtcdDbgDialer provides a default implementation of the EtcdDbgDialer interface.
type DefaultEtcdDbgDialer struct{}

func (DefaultEtcdDbgDialer) LookupIP(ctx context.Context, hostname string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", hostname)
}

func (DefaultEtcdDbgDialer) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

// EtcdDbg performs a set of sanity checks concerning the connection to the given
// etcd cluster, and outputs the result in a user-friendly format.
func EtcdDbg(ctx context.Context, cfgfile string, dialer EtcdDbgDialer, w io.Writer) {
	iw := newIndentedWriter(w, 0)

	iw.Println("📄 Configuration path: %s", cfgfile)
	cfg, err := clientyaml.NewConfig(cfgfile)
	if err != nil {
		iw.Println("❌ Cannot parse etcd configuration: %s", err)
		return
	}

	iw.NewLine()
	if len(cfg.Endpoints) == 0 {
		iw.Println("❌ No available endpoints")
	} else {
		iw.Println("🔌 Endpoints:")
		for _, ep := range cfg.Endpoints {
			iiw := iw.WithExtraIndent(3)
			iiw.Println("- %s", ep)
			etcdDbgEndpoint(ctx, ep, cfg.TLS.Clone(), dialer, iiw.WithExtraIndent(2))
		}
	}

	iw.NewLine()
	iw.Println("🔑 Digital certificates:")
	etcdDbgCerts(cfgfile, cfg, iw.WithExtraIndent(3))

	iw.NewLine()
	iw.Println("⚙️ Etcd client:")
	iiw := iw.WithExtraIndent(3)
	cfg.Context = ctx
	cfg.Logger = zap.NewNop()
	cfg.DialOptions = append(cfg.DialOptions, grpc.WithContextDialer(dialer.DialContext))

	cl, err := client.New(*cfg)
	if err != nil {
		iiw.Println("❌ Failed to create etcd client: %s", err)
		return
	}
	defer cl.Close()

	// Try to retrieve the heartbeat key, as a basic authorization check.
	// It doesn't really matter whether the heartbeat key exists or not.
	// Client.New() does not block on connection failure, and hence
	// we need to check the connection state to determine the type of failure.
	ctxGet, cancelGet := context.WithTimeout(ctx, 1*time.Second)
	defer cancelGet()
	out, err := cl.Get(ctxGet, HeartbeatPath)
	if err != nil {
		if cl.ActiveConnection().GetState() == connectivity.TransientFailure {
			iiw.Println("❌ Failed to establish connection: %s", err)
		} else {
			iiw.Println("❌ Failed to retrieve key from etcd: %s", err)
		}
		return
	}

	iiw.Println("✅ Etcd connection successfully established")
	if out.Header != nil {
		iiw.Println("ℹ️  Etcd cluster ID: %x", out.Header.GetClusterId())
	}
}

func etcdDbgEndpoint(ctx context.Context, ep string, tlscfg *tls.Config, dialer EtcdDbgDialer, iw *indentedWriter) {
	u, err := url.Parse(ep)
	if err != nil {
		iw.Println("❌ Cannot parse endpoint: %s", err)
		return
	}

	// Hostname resolution
	hostname := u.Hostname()
	if net.ParseIP(hostname) == nil {
		ips, err := dialer.LookupIP(ctx, hostname)
		if err != nil {
			iw.Println("❌ Cannot resolve hostname: %s", err)
		} else {
			iw.Println("✅ Hostname resolved to: %s", etcdDbgOutputIPs(ips))
		}
	}

	// TCP Connection
	conn, err := dialer.DialContext(ctx, u.Host)
	if err != nil {
		iw.Println("❌ Cannot establish TCP connection to %s: %s", u.Host, err)
		return
	}

	iw.Println("✅ TCP connection successfully established to %s", conn.RemoteAddr())
	if u.Scheme != "https" {
		conn.Close()
		return
	}

	// TLS Connection
	if tlscfg.ServerName == "" {
		tlscfg.ServerName = hostname
	}

	// We use GetClientCertificate rather than Certificates to return an error
	// in case the certificate does not match any of the requested CAs. One
	// limitation, though, is that the match appears to be performed based on
	// the distinguished name only, and it doesn't fail if two CAs have the same
	// DN (which is typically the case with the default CA generated by Cilium).
	var acceptableCAs [][]byte
	tlscfg.GetClientCertificate = func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		for _, chain := range tlscfg.Certificates {
			if err := cri.SupportsCertificate(&chain); err == nil {
				return &chain, nil
			}
		}

		acceptableCAs = cri.AcceptableCAs
		return nil, fmt.Errorf("client certificate is not signed by any acceptable CA")
	}

	tconn := tls.Client(conn, tlscfg)
	defer tconn.Close()

	err = tconn.HandshakeContext(ctx)
	if err != nil {
		iw.Println("❌ Cannot establish TLS connection to %s: %s", u.Host, err)
		if len(acceptableCAs) > 0 {
			// The output is suboptimal being DER-encoded, but there doesn't
			// seem to be any easy way to parse it (the utility used by
			// ParseCertificate is not exported). Better than nothing though.
			var buf bytes.Buffer
			for i, ca := range acceptableCAs {
				if i != 0 {
					buf.WriteString(", ")
				}
				buf.WriteRune('"')
				buf.WriteString(string(ca))
				buf.WriteRune('"')
			}

			iw.Println("ℹ️  Acceptable CAs: %s", buf.String())
		}
		return
	}

	iw.Println("✅ TLS connection successfully established to %s", tconn.RemoteAddr())
	iw.Println("ℹ️  Negotiated TLS version: %s, ciphersuite %s",
		tls.VersionName(tconn.ConnectionState().Version),
		tls.CipherSuiteName(tconn.ConnectionState().CipherSuite))

	// With TLS 1.3, the server doesn't acknowledge whether client authentication
	// succeeded, and a possible error is returned only when reading some data.
	// Hence, let's trigger a request, so that we see if it failed.
	tconn.SetDeadline(time.Now().Add(1 * time.Second))
	data := fmt.Sprintf("GET /version HTTP/1.1\r\nHost: %s\r\n\r\n", u.Host)
	_, err = tconn.Write([]byte(data))
	if err != nil {
		iw.Println("❌ Failed to perform a GET /version request: %s", err)
		return
	}

	buf := make([]byte, 1000)
	_, err = tconn.Read(buf)
	if err != nil {
		opErr := &net.OpError{}
		if errors.As(err, &opErr) && opErr.Op == "remote error" {
			iw.Println("❌ TLS client authentication failed: %s", err)
		} else {
			iw.Println("❌ Failed to retrieve GET /version answer: %s", err)
		}
		return
	}

	matches := etcdVersionRegexp.FindAllStringSubmatch(string(buf), 1)
	if len(matches) != 1 {
		iw.Println("⚠️ Could not retrieve etcd server version")
		return
	}

	iw.Println("ℹ️  Etcd server version: %s", matches[0][etcdVersionRegexp.SubexpIndex("version")])
}

func etcdDbgCerts(cfgfile string, cfg *client.Config, iw *indentedWriter) {
	if cfg.TLS.RootCAs == nil {
		iw.Println("⚠️ Root CA unset: using system pool")
	} else {
		// Retrieve the RootCA path from the configuration, as it appears
		// that we cannot introspect cfg.TLS.RootCAs.
		certs, err := etcdDbgRetrieveRootCAFile(cfgfile)
		if err != nil {
			iw.Println("❌ Failed to retrieve Root CA path: %s", err)
		} else {
			iw.Println("✅ TLS Root CA certificates:")
			for _, cert := range certs {
				parsed, err := x509.ParseCertificate(cert)
				if err != nil {
					iw.Println("❌ Failed to parse certificate: %s", err)
					continue
				}

				etcdDbgOutputCert(parsed, iw.WithExtraIndent(3))
			}
		}
	}

	if len(cfg.TLS.Certificates) == 0 {
		iw.Println("⚠️ No available TLS client certificates")
	} else {
		iw.Println("✅ TLS client certificates:")
		for _, cert := range cfg.TLS.Certificates {
			if len(cert.Certificate) == 0 {
				iw.Println("❌ The certificate looks invalid")
				continue
			}

			leaf, err := x509.ParseCertificate(cert.Certificate[0])
			if err != nil {
				iw.Println("❌ Failed to parse certificate: %s", err)
				continue
			}

			iiw := iw.WithExtraIndent(3)
			etcdDbgOutputCert(leaf, iiw)
			iiw = iiw.WithExtraIndent(2)

			// Print intermediate certificates, if any.
			intermediates := x509.NewCertPool()
			for _, cert := range cert.Certificate[1:] {
				iiw.Println("Intermediates:")

				intermediate, err := x509.ParseCertificate(cert)
				if err != nil {
					iw.Println("❌ Failed to parse intermediate certificate: %s", err)
					continue
				}

				etcdDbgOutputCert(intermediate, iiw)
				intermediates.AddCert(intermediate)
			}

			// Attempt to verify whether the given certificate can be validated
			// using the configured root CAs. Although a failure is not necessarily
			// an error, as the remote etcd server may be configured with a different
			// root CA, it still signals a misconfiguration in most cases.
			opts := x509.VerifyOptions{
				Roots:         cfg.TLS.RootCAs,
				Intermediates: intermediates,
			}

			_, err = leaf.Verify(opts)
			if err != nil {
				iiw.Println("⚠️ Cannot verify certificate with the configured root CAs")
			}
		}
	}

	if cfg.Username != "" {
		passwd := "unset"
		if cfg.Password != "" {
			passwd = "set"
		}

		iw.Println("✅ Username set to %s, password is %s", cfg.Username, passwd)
	}
}

func etcdDbgOutputIPs(ips []net.IP) string {
	var buf bytes.Buffer
	for i, ip := range ips {
		if i > 0 {
			buf.WriteString(", ")
		}

		if i == 4 {
			buf.WriteString("...")
			break
		}

		buf.WriteString(ip.String())
	}
	return buf.String()
}

func etcdDbgRetrieveRootCAFile(cfgfile string) (certs [][]byte, err error) {
	var yc struct {
		TrustedCAfile string `json:"trusted-ca-file"`
	}

	b, err := os.ReadFile(cfgfile)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(b, &yc)
	if err != nil {
		return nil, err
	}

	if yc.TrustedCAfile == "" {
		return nil, errors.New("not provided")
	}

	data, err := os.ReadFile(yc.TrustedCAfile)
	if err != nil {
		return nil, err
	}

	for {
		block, rest := pem.Decode(data)
		if block == nil {
			if len(certs) == 0 {
				return nil, errors.New("no certificate found")
			}

			return certs, nil
		}

		if block.Type == "CERTIFICATE" {
			certs = append(certs, block.Bytes)
		}

		data = rest
	}
}

func etcdDbgOutputCert(cert *x509.Certificate, iw *indentedWriter) {
	sn := cert.SerialNumber.Text(16)
	for i := 2; i < len(sn); i += 3 {
		sn = sn[:i] + ":" + sn[i:]
	}

	iw.Println("- Serial number:       %s", string(sn))
	iw.Println("  Subject:             %s", cert.Subject)
	iw.Println("  Issuer:              %s", cert.Issuer)
	iw.Println("  Validity:")
	iw.Println("    Not before:  %s", cert.NotBefore)
	iw.Println("    Not after:   %s", cert.NotAfter)
}

type indentedWriter struct {
	w      io.Writer
	indent []byte
}

func newIndentedWriter(w io.Writer, indent int) *indentedWriter {
	return &indentedWriter{w: w, indent: []byte(strings.Repeat(" ", indent))}
}

func (iw *indentedWriter) NewLine() { iw.w.Write([]byte("\n")) }

func (iw *indentedWriter) Println(format string, a ...any) {
	iw.w.Write(iw.indent)
	fmt.Fprintf(iw.w, format, a...)
	iw.NewLine()
}

func (iw *indentedWriter) WithExtraIndent(indent int) *indentedWriter {
	return newIndentedWriter(iw.w, len(iw.indent)+indent)
}
