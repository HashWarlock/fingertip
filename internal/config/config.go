package config

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"github.com/buffrr/letsdane"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

const AppName = "Fingertip"
const AppId = "com.impervious.fingertip"
const CertFileName = "fingertip.crt"
const CertKeyFileName = "private.key"
const CertName = "DNSSEC"

type App struct {
	Path        string
	CertPath    string
	keyPath     string
	DNSProcPath string
	Proxy       letsdane.Config
	ProxyAddr   string
}

func getOrCreateDir() (string, error) {
	home, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}

	p := path.Join(home, AppName)
	if _, err := os.Stat(p); err != nil {
		if err := os.Mkdir(p, 0700); err != nil {
			return "", err
		}
	}

	return p, nil
}

func (c *App) getOrCreateCA() (string, string, error) {
	certPath := path.Join(c.Path, CertFileName)
	keyPath := path.Join(c.Path, CertKeyFileName)

	if _, err := os.Stat(certPath); err != nil {
		if _, err := os.Stat(keyPath); err != nil {
			ca, priv, err := letsdane.NewAuthority(CertName, CertName, 365*24*time.Hour, nameConstraints)
			if err != nil {
				return "", "", fmt.Errorf("couldn't generate CA: %v", err)
			}

			certOut, err := os.OpenFile(certPath, os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return "", "", fmt.Errorf("couldn't create CA file: %v", err)
			}
			defer certOut.Close()

			pem.Encode(certOut, &pem.Block{
				Type:  "CERTIFICATE",
				Bytes: ca.Raw,
			})

			privOut := bytes.NewBuffer([]byte{})
			pem.Encode(privOut, &pem.Block{
				Type:  "RSA PRIVATE KEY",
				Bytes: x509.MarshalPKCS1PrivateKey(priv),
			})

			kOut, err := os.OpenFile(keyPath, os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				return "", "", fmt.Errorf("couldn't create CA private key file: %v", err)
			}
			defer kOut.Close()

			kOut.Write(privOut.Bytes())
			return certPath, keyPath, nil
		}
	}
	return certPath, keyPath, nil
}

func loadX509KeyPair(certFile, keyFile string) (tls.Certificate, error) {
	certPEMBlock, err := ioutil.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, err
	}

	keyPEMBlock, err := ioutil.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.X509KeyPair(certPEMBlock, keyPEMBlock)
}

func (c *App) loadCA() (*x509.Certificate, interface{}, error) {
	var x509c *x509.Certificate
	var priv interface{}
	var err error

	c.CertPath, c.keyPath, err = c.getOrCreateCA()
	if err != nil {
		return nil, nil, err
	}

	var cert tls.Certificate
	if cert, err = loadX509KeyPair(c.CertPath, c.keyPath); err != nil {
		return nil, nil, err
	}

	priv = cert.PrivateKey
	if x509c, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
		return nil, nil, err
	}

	return x509c, priv, nil
}

func getPACScript(proxyAddr string, names []string) string {
	skippedNames := fmt.Sprintf(`'%s'`, strings.Join(names, `', '`))

	pac := fmt.Sprintf(`
function FindProxyForURL(url, host) {
    var skipped = [ %s ];

    // skip any TLD in the list 
    var tld = host;
    var lastDot = tld.lastIndexOf('.');
    if (lastDot != -1) {
      tld = tld.substr(lastDot+1);
    }
    tld = tld.toLowerCase();

    if (skipped.includes(tld)) {
      return 'DIRECT';
    }

    // skip IP addresses
    var isIpV4Addr = /^(\d+.){3}\d+$/;
    if (isIpV4Addr.test(host)) {
       return "DIRECT";
    }

    // loosely check if IPv6
    if (lastDot == -1 && host.split(':').length > 2) {
      return "DIRECT";
    }

    return "PROXY %s";
}
`, skippedNames, proxyAddr)

	return pac
}

func NewConfig() (*App, error) {
	var err error
	c := &App{}
	if c.Path, err = getOrCreateDir(); err != nil {
		return nil, fmt.Errorf("failed creating config: %v", err)
	}

	c.Proxy.Constraints = nameConstraints
	c.Proxy.SkipNameChecks = false
	c.Proxy.Verbose = true
	c.Proxy.Validity = time.Hour
	c.Proxy.ContentHandler = &contentHandler{c}
	if c.Proxy.Certificate, c.Proxy.PrivateKey, err = c.loadCA(); err != nil {
		return nil, fmt.Errorf("failed creating config: %v", err)
	}

	return c, nil
}

type contentHandler struct {
	config *App
}

func (c *contentHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "" || req.URL.Path == "/" {
		url := GetProxyURL(c.config.ProxyAddr)
		onBoardingTmpl.Execute(rw, onBoardingTmplData{
			CertPath: c.config.CertPath,
			CertLink: url + "/" + CertFileName,
			PACLink:  url + "/proxy.pac",
		})
		return
	}

	if req.URL.Path == "/"+CertFileName {
		rw.Header().Set("Content-Type", "application/x-x509-ca-cert")
		rw.Write(pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: c.config.Proxy.Certificate.Raw,
		}))

		return
	}

	if req.URL.Path == "/proxy.pac" {
		rw.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		var names []string
		for n := range nameConstraints {
			names = append(names, n)
		}
		fmt.Fprint(rw, getPACScript(c.config.ProxyAddr, names))
		return
	}
}

func GetProxyURL(addr string) string {
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) < 2 {
		return addr
	}

	if parts[0] == "" {
		parts[0] = "127.0.0.1"
	}

	return "http://" + strings.Join(parts, ":")
}

func init() {
	// Skip reserved names in RFC2606
	// https://datatracker.ietf.org/doc/html/rfc2606
	var testNames = []string{"localhost", "test", "invalid", "example"}
	for _, name := range testNames {
		nameConstraints[name] = struct{}{}
	}
}
