package psrp

import (
	"errors"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
	"github.com/smnsjas/go-psrp/client"
)

// TransportType represents the PSRP transport mechanism
type TransportType string

const (
	// TransportWSMan uses WS-Management protocol (HTTP/HTTPS)
	TransportWSMan TransportType = "wsman"
	// TransportHvSocket uses Hyper-V sockets (PowerShell Direct)
	TransportHvSocket TransportType = "hvsock"
)

// AuthType represents the authentication method
type AuthType string

const (
	AuthBasic     AuthType = "basic"
	AuthNTLM      AuthType = "ntlm"
	AuthKerberos  AuthType = "kerberos"
	AuthNegotiate AuthType = "negotiate"
)

// Config is the configuration structure for the PSRP communicator.
type Config struct {
	// Type is always "psrp" for this communicator
	Type string `mapstructure:"communicator"`

	// Connection settings
	PSRPHost     string        `mapstructure:"psrp_host"`
	PSRPPort     int           `mapstructure:"psrp_port"`
	PSRPUsername string        `mapstructure:"psrp_username"`
	PSRPPassword string        `mapstructure:"psrp_password"`
	PSRPTimeout  time.Duration `mapstructure:"psrp_timeout"`

	// Transport configuration
	PSRPTransport         TransportType `mapstructure:"psrp_transport"`
	PSRPVMID              string        `mapstructure:"psrp_vmid"`               // For HvSocket transport
	PSRPConfigurationName string        `mapstructure:"psrp_configuration_name"` // PowerShell config name (HvSocket)

	// TLS/SSL settings
	PSRPUseTLS             bool `mapstructure:"psrp_use_tls"`
	PSRPInsecureSkipVerify bool `mapstructure:"psrp_insecure"`

	// Authentication
	PSRPAuthType AuthType `mapstructure:"psrp_auth_type"`
	PSRPDomain   string   `mapstructure:"psrp_domain"` // For NTLM and Negotiate
	PSRPRealm    string   `mapstructure:"psrp_realm"`  // For Kerberos (optional on Windows/SSPI)

	// Kerberos-specific (Unix/gokrb5 path; ignored on Windows when SSPI is used)
	PSRPKrb5ConfPath string `mapstructure:"psrp_krb5_conf_path"` // Defaults to /etc/krb5.conf on Unix
	PSRPKeytabPath   string `mapstructure:"psrp_keytab_path"`
	PSRPCCachePath   string `mapstructure:"psrp_ccache_path"`

	// Advanced settings
	PSRPIdleTimeout         string        `mapstructure:"psrp_idle_timeout"` // ISO8601 duration (e.g., "PT30M")
	PSRPMaxRunspaces        int           `mapstructure:"psrp_max_runspaces"`
	PSRPKeepAliveInterval   time.Duration `mapstructure:"psrp_keepalive_interval"`
	PSRPRunspaceOpenTimeout time.Duration `mapstructure:"psrp_runspace_open_timeout"`

	ctx interpolate.Context
}

// NewConfig returns a Config with default values.
// Defaults match go-psrp's DefaultConfig() where applicable.
func NewConfig() *Config {
	return &Config{
		Type:                    "psrp",
		PSRPPort:                5985,
		PSRPTimeout:             5 * time.Minute,
		PSRPTransport:           TransportWSMan,
		PSRPUseTLS:              false,
		PSRPInsecureSkipVerify:  false,
		PSRPAuthType:            AuthNegotiate, // go-psrp default
		PSRPIdleTimeout:         "PT30M",
		PSRPMaxRunspaces:        1,
		PSRPKeepAliveInterval:   0, // Disabled by default
		PSRPRunspaceOpenTimeout: 60 * time.Second,
	}
}

// Prepare validates the configuration
func (c *Config) Prepare(ctx *interpolate.Context) []error {
	if ctx != nil {
		c.ctx = *ctx
	}

	var errs []error

	// Apply defaults for unset values
	if c.PSRPPort == 0 {
		c.PSRPPort = 5985
	}
	if c.PSRPTimeout == 0 {
		c.PSRPTimeout = 5 * time.Minute
	}
	if c.PSRPTransport == "" {
		c.PSRPTransport = TransportWSMan
	}
	if c.PSRPAuthType == "" {
		c.PSRPAuthType = AuthNegotiate
	}

	// Validate authentication type
	switch c.PSRPAuthType {
	case AuthBasic, AuthNTLM:
		// Basic and NTLM always require explicit credentials (no SSO path)
		if c.PSRPUsername == "" {
			errs = append(errs, errors.New("psrp_username is required for basic/ntlm authentication"))
		}
	case AuthKerberos, AuthNegotiate:
		// On Windows, go-psrp uses SSPI which supports SSO with the logged-in
		// user's credentials. Username can be empty to trigger SSO.
		// On Unix, go-psrp uses gokrb5 (pure Go) which requires explicit
		// credentials (password, keytab, or ccache) and krb5.conf.
		// We don't validate username here because go-psrp's Config.Validate()
		// handles the platform-specific check via auth.SupportsSSO().
	default:
		errs = append(errs, errors.New("psrp_auth_type must be 'basic', 'ntlm', 'kerberos', or 'negotiate'"))
	}

	// Transport-specific validation
	switch c.PSRPTransport {
	case TransportWSMan:
		if c.PSRPHost == "" {
			errs = append(errs, errors.New("psrp_host is required for wsman transport"))
		}
		// Set default TLS port if needed
		if c.PSRPUseTLS && c.PSRPPort == 5985 {
			c.PSRPPort = 5986
		}
	case TransportHvSocket:
		if c.PSRPVMID == "" {
			errs = append(errs, errors.New("psrp_vmid is required for hvsock transport"))
		}
	default:
		errs = append(errs, errors.New("psrp_transport must be 'wsman' or 'hvsock'"))
	}

	return errs
}

// ToGoPSRPConfig converts the Packer config to a go-psrp client.Config.
// Returns by value to match client.New(hostname, Config) signature.
func (c *Config) ToGoPSRPConfig() client.Config {
	cfg := client.DefaultConfig()

	// Basic connection settings
	cfg.Username = c.PSRPUsername
	cfg.Password = c.PSRPPassword
	cfg.Port = c.PSRPPort
	cfg.UseTLS = c.PSRPUseTLS
	cfg.InsecureSkipVerify = c.PSRPInsecureSkipVerify
	cfg.Timeout = c.PSRPTimeout

	// Transport
	switch c.PSRPTransport {
	case TransportWSMan:
		cfg.Transport = client.TransportWSMan
	case TransportHvSocket:
		cfg.Transport = client.TransportHvSocket
		cfg.VMID = c.PSRPVMID
		cfg.ConfigurationName = c.PSRPConfigurationName
	}

	// Authentication
	// On Windows, Kerberos/Negotiate use SSPI natively when Username is empty
	// (SSO with logged-in user). The Krb5/Keytab/CCachePath fields are for
	// the Unix gokrb5 (pure Go) path, or Windows fallback when SSPI isn't used.
	switch c.PSRPAuthType {
	case AuthBasic:
		cfg.AuthType = client.AuthBasic
	case AuthNTLM:
		cfg.AuthType = client.AuthNTLM
		cfg.Domain = c.PSRPDomain
	case AuthKerberos:
		cfg.AuthType = client.AuthKerberos
		cfg.Domain = c.PSRPDomain
		cfg.Realm = c.PSRPRealm
		cfg.Krb5ConfPath = c.PSRPKrb5ConfPath
		cfg.KeytabPath = c.PSRPKeytabPath
		cfg.CCachePath = c.PSRPCCachePath
	case AuthNegotiate:
		cfg.AuthType = client.AuthNegotiate
		cfg.Domain = c.PSRPDomain
		cfg.Realm = c.PSRPRealm
		cfg.Krb5ConfPath = c.PSRPKrb5ConfPath
		cfg.KeytabPath = c.PSRPKeytabPath
		cfg.CCachePath = c.PSRPCCachePath
	}

	// Advanced settings
	cfg.IdleTimeout = c.PSRPIdleTimeout
	cfg.MaxRunspaces = c.PSRPMaxRunspaces
	cfg.KeepAliveInterval = c.PSRPKeepAliveInterval
	cfg.RunspaceOpenTimeout = c.PSRPRunspaceOpenTimeout

	return cfg
}
