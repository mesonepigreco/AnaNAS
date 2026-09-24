package helper

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	config    Config
	client    tls.Certificate
	roots     *x509.CertPool
	pin, file string
}

func helperFixture(t *testing.T) fixture {
	t.Helper()
	work := filepath.Join(t.TempDir(), "nas-sync-capability-test", "ananas-helper-test-"+strings.Repeat("a", 32))
	for _, dir := range []string{work, filepath.Join(work, "root"), filepath.Join(work, "state")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	unique := []int{}
	seen := map[int]bool{}
	for _, group := range groups {
		if !seen[group] {
			unique = append(unique, group)
			seen[group] = true
		}
	}
	c := Config{Root: filepath.Join(work, "root"), StateDir: filepath.Join(work, "state"), Namespace: strings.Repeat("a", 64), Listen: "127.0.0.1:9876", Interface: "lo", Prefix: "127.0.0.0/8", Peers: []string{"127.0.0.1"}, UID: os.Getuid(), GID: os.Getgid(), Groups: unique, Certificate: filepath.Join(work, "server.pem"), PrivateKey: filepath.Join(work, "server.key"), ClientCA: filepath.Join(work, "ca.pem"), MaxConnections: 2, MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, ReadBytesPerSecond: 1 << 30}
	c.MaxCacheBytes, c.MaxCacheEntries = 32<<20, 128
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(n int64, usage x509.ExtKeyUsage) (tls.Certificate, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(n), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	}
	server, key := issue(2, x509.ExtKeyUsageServerAuth)
	client, _ := issue(3, x509.ExtKeyUsageClientAuth)
	serverPin, clientPin := sha256.Sum256(server.Certificate[0]), sha256.Sum256(client.Certificate[0])
	c.Clients = map[string]string{hex.EncodeToString(clientPin[:]): strings.Repeat("b", 64)}
	for path, data := range map[string][]byte{c.Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate[0]}), c.PrivateKey: key, c.ClientCA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(work, "helper.json")
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return fixture{c, client, pool, hex.EncodeToString(serverPin[:]), file}
}

func TestHelperPrivateConfigurationAndReadOnlyPreflight(t *testing.T) {
	f := helperFixture(t)
	c, err := Load(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckFiles(c); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(c.StateDir); err != nil || len(entries) != 0 {
		t.Fatal("preflight created state", entries, err)
	}
	if err := c.CheckDisposable(); err != nil {
		t.Fatal(err)
	}
	c.Root = filepath.Join(filepath.Dir(c.Root), "existing-user-files")
	if err := c.CheckDisposable(); err == nil {
		t.Fatal("arbitrary visible root accepted for write test")
	}
	if err := os.Chmod(f.config.PrivateKey, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.config.TLS(); err == nil {
		t.Fatal("non-private key accepted")
	}
}

func TestHelperRejectsSymlinkConfigAndUnknownWriteSwitch(t *testing.T) {
	f := helperFixture(t)
	link := filepath.Join(filepath.Dir(f.file), "link.json")
	if err := os.Symlink(f.file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("symlink configuration accepted")
	}
	data, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	data = append([]byte(`{"writes":true,`), data[1:]...)
	if err := os.WriteFile(f.file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(f.file); err == nil {
		t.Fatal("production writes enabled through unknown switch")
	}
}

func TestHelperRejectsUnsafeNetworkAndIdentityConfiguration(t *testing.T) {
	for _, mode := range []string{"wildcard", "hostname", "public", "peer", "wide subnet", "noncanonical subnet", "duplicate subnet", "identity", "overlap"} {
		t.Run(mode, func(t *testing.T) {
			c := helperFixture(t).config
			switch mode {
			case "wildcard":
				c.Listen = "0.0.0.0:9876"
			case "hostname":
				c.Listen = "localhost:9876"
			case "public":
				c.Listen = "8.8.8.8:9876"
			case "peer":
				c.Peers = []string{"10.23.42.17"}
			case "wide subnet":
				c.Peers = []string{"0.0.0.0/0"}
			case "noncanonical subnet":
				c.Peers = []string{"127.0.0.1/8"}
			case "duplicate subnet":
				c.Peers = []string{"127.0.0.0/8", "127.0.0.0/8"}
			case "identity":
				c.UID = 0
			case "overlap":
				c.StateDir = filepath.Join(c.Root, "state")
			}
			if err := c.Validate(); err == nil {
				t.Fatal("unsafe helper configuration accepted")
			}
		})
	}
}

func TestHelperAcceptsDirectClientSubnet(t *testing.T) {
	c := helperFixture(t).config
	c.Peers = []string{"127.0.0.0/8"}
	if err := c.Validate(); err != nil {
		t.Fatal("direct client subnet rejected:", err)
	}
}

func TestHelperResolvesCurrentAddressListener(t *testing.T) {
	c := helperFixture(t).config
	c.Listen = ":9876"
	if err := c.Validate(); err != nil {
		t.Fatal("current-address listener rejected:", err)
	}
	resolved, err := c.Resolved()
	if err != nil || resolved.Listen != "127.0.0.1:9876" {
		t.Fatal(resolved.Listen, err)
	}
	for _, listen := range []string{":", ":0", ":09876", ":x"} {
		c.Listen = listen
		if err := c.Validate(); err == nil {
			t.Fatal("accepted listener", listen)
		}
	}
}

func TestInheritedListenerDoesNotCloseUnrelatedDescriptor(t *testing.T) {
	f := helperFixture(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if _, err := InheritedListener(int(reader.Fd()), f.config); err == nil {
		t.Fatal("pipe accepted as listener")
	}
	if _, err := writer.Write([]byte{42}); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := reader.Read(b[:]); err != nil || b[0] != 42 {
		t.Fatal("unrelated descriptor was closed", err)
	}
}
