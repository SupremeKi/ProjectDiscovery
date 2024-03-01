package authprovider

import (
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/projectdiscovery/nuclei/v3/pkg/authprovider/authx"
	errorutil "github.com/projectdiscovery/utils/errors"
	urlutil "github.com/projectdiscovery/utils/url"
)

// FileAuthProvider is an auth provider for file based auth
// it accepts a secrets file and returns its provider
type FileAuthProvider struct {
	Path     string
	store    *authx.Authx
	compiled map[*regexp.Regexp]authx.AuthStrategy
	domains  map[string]authx.AuthStrategy
}

// NewFileAuthProvider creates a new file based auth provider
func NewFileAuthProvider(path string) (AuthProvider, error) {
	store, err := authx.GetAuthDataFromFile(path)
	if err != nil {
		return nil, err
	}
	if len(store.Secrets) == 0 {
		return nil, ErrNoSecrets
	}
	for _, secret := range store.Secrets {
		if err := secret.Validate(); err != nil {
			return nil, errorutil.NewWithErr(err).Msgf("invalid secret in file: %s", path)
		}
	}

	f := &FileAuthProvider{Path: path, store: store}
	f.init()
	return f, nil
}

// init initializes the file auth provider
func (f *FileAuthProvider) init() {
	for _, secret := range f.store.Secrets {
		if len(secret.DomainsRegex) > 0 {
			for _, domain := range secret.DomainsRegex {
				if f.compiled == nil {
					f.compiled = make(map[*regexp.Regexp]authx.AuthStrategy)
				}
				compiled, err := regexp.Compile(domain)
				if err != nil {
					continue
				}
				f.compiled[compiled] = secret.GetStrategy()
			}
		}
		for _, domain := range secret.Domains {
			if f.domains == nil {
				f.domains = make(map[string]authx.AuthStrategy)
			}
			f.domains[strings.TrimSpace(domain)] = secret.GetStrategy()
		}
	}
}

// LookupAddr looks up a given domain/address and returns appropriate auth strategy
func (f *FileAuthProvider) LookupAddr(addr string) authx.AuthStrategy {
	if strings.Contains(addr, ":") {
		// default normalization for host:port
		host, port, err := net.SplitHostPort(addr)
		if err == nil && (port == "80" || port == "443") {
			addr = host
		}
	}
	for domain, strategy := range f.domains {
		if strings.EqualFold(domain, addr) {
			return strategy
		}
	}
	for compiled, strategy := range f.compiled {
		if compiled.MatchString(addr) {
			return strategy
		}
	}
	return nil
}

// LookupURL looks up a given URL and returns appropriate auth strategy
func (f *FileAuthProvider) LookupURL(u *url.URL) authx.AuthStrategy {
	return f.LookupAddr(u.Host)
}

// LookupURLX looks up a given URL and returns appropriate auth strategy
func (f *FileAuthProvider) LookupURLX(u *urlutil.URL) authx.AuthStrategy {
	return f.LookupAddr(u.Host)
}
