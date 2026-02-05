# packer-plugin-psrp

A Go library that implements a PowerShell Remoting Protocol (PSRP) communicator for [Packer](https://www.packer.io). Builder plugins import this package to provision Windows machines over native PSRP instead of WinRM.

> **This is not a standalone Packer plugin.** Packer's plugin system has no way to register communicators independently. Builders must import the `communicator/psrp` package and wire it into the SDK's `CustomConnect` map.

## Why PSRP Instead of WinRM?

1. **Native PowerShell objects** — Direct PSRP protocol support preserves structured output instead of flattening everything to text.
2. **PowerShell Direct** — Hyper-V socket transport enables secure, network-free communication with VMs (no firewall rules needed).
3. **Modern authentication** — Full Kerberos/Negotiate support with platform-aware SSO on Windows (SSPI) and pure-Go gokrb5 on Unix.

## Builder Integration

### Install

```bash
go get github.com/jasonsimons/packer-plugin-psrp@latest
```

### Wire into CustomConnect

The SDK's `communicator.StepConnect` has a `CustomConnect map[string]multistep.Step` field. Register a `psrp.StepConnect` under the key `"psrp"`:

```go
import (
    "github.com/hashicorp/packer-plugin-sdk/communicator"
    "github.com/hashicorp/packer-plugin-sdk/multistep"
    "github.com/jasonsimons/packer-plugin-psrp/communicator/psrp"
)

// In your builder's Run() method, when building the step sequence:
commStep := &communicator.StepConnect{
    Config: &b.config.CommConfig,
    Host:   hostFunc,
    CustomConnect: map[string]multistep.Step{
        "psrp": &psrp.StepConnect{
            Config: &b.config.PSRPConfig, // *psrp.Config
            Host:   hostFunc,
        },
    },
}
```

### SDK Config.Prepare() Gotcha

The SDK's `communicator.Config.Prepare()` rejects any communicator type it doesn't recognize (only `ssh`, `winrm`, `docker`, `dockerWindowsContainer`, `none` are accepted). If a user sets `communicator = "psrp"`, the SDK will error before your builder gets a chance to use it.

Your builder must handle the `"psrp"` type **before** calling the SDK's Prepare:

```go
func (b *Builder) Prepare(raws ...interface{}) ([]string, []string, error) {
    // ... decode config ...

    // Validate PSRP config ourselves (SDK would reject "psrp" type)
    if b.config.CommConfig.Type == "psrp" {
        errs := b.config.PSRPConfig.Prepare(&b.config.ctx)
        if len(errs) > 0 {
            // handle errors
        }
        // Skip SDK's communicator.Config.Prepare() for the type check,
        // or temporarily swap the type to "none" and restore it after.
    }
}
```

## Configuration Reference

These are the HCL options your builder's users will set. All field names use `mapstructure` tags for HCL parsing.

### Connection

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `psrp_host` | string | *(required for wsman)* | Hostname or IP address. Builders typically override this via `StepConnect.Host()` |
| `psrp_port` | int | `5985` (auto `5986` when TLS enabled) | Port number |
| `psrp_username` | string | *(required for basic/ntlm; optional for kerberos/negotiate)* | Username |
| `psrp_password` | string | | Password |
| `psrp_timeout` | duration | `5m` | Connection timeout with retry |

### Transport

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `psrp_transport` | string | `wsman` | `"wsman"` (HTTP/HTTPS) or `"hvsock"` (Hyper-V sockets) |
| `psrp_vmid` | string | *(required for hvsock)* | Hyper-V VM ID (UUID) |
| `psrp_configuration_name` | string | | PowerShell configuration name (hvsock) |

### TLS

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `psrp_use_tls` | bool | `false` | Use HTTPS instead of HTTP |
| `psrp_insecure` | bool | `false` | Skip TLS certificate verification |

### Authentication

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `psrp_auth_type` | string | `negotiate` | `"basic"`, `"ntlm"`, `"kerberos"`, or `"negotiate"` |
| `psrp_domain` | string | | Domain for NTLM/Negotiate/Kerberos |
| `psrp_realm` | string | | Kerberos realm (auto-detected from krb5.conf if empty) |
| `psrp_krb5_conf_path` | string | `/etc/krb5.conf` | Path to krb5.conf (Unix only) |
| `psrp_keytab_path` | string | | Kerberos keytab file path |
| `psrp_ccache_path` | string | | Kerberos credential cache path |

**Kerberos/Negotiate on Windows**: Leave `psrp_username` empty to use SSO with the logged-in user's credentials (SSPI). On Unix, explicit credentials are always required.

### Advanced

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `psrp_idle_timeout` | string | `PT30M` | Server-side idle timeout (ISO 8601) |
| `psrp_max_runspaces` | int | `1` | Maximum concurrent runspaces |
| `psrp_keepalive_interval` | duration | `0` (disabled) | PSRP keepalive interval |
| `psrp_runspace_open_timeout` | duration | `60s` | Timeout for opening a runspace |

## HCL Examples

### Basic (WSMan/HTTP)

```hcl
source "your-builder" "example" {
  communicator = "psrp"

  psrp_host      = "192.168.1.100"
  psrp_username  = "Administrator"
  psrp_password  = "P@ssw0rd"
  psrp_auth_type = "basic"
}
```

### TLS with NTLM

```hcl
source "your-builder" "example" {
  communicator = "psrp"

  psrp_host      = "server.domain.com"
  psrp_port      = 5986
  psrp_username  = "domain\\user"
  psrp_password  = "P@ssw0rd"
  psrp_use_tls   = true
  psrp_auth_type = "ntlm"
  psrp_domain    = "DOMAIN"
}
```

### Kerberos

```hcl
source "your-builder" "example" {
  communicator = "psrp"

  psrp_host           = "server.domain.com"
  psrp_username       = "user"
  psrp_password       = "P@ssw0rd"
  psrp_use_tls        = true
  psrp_auth_type      = "kerberos"
  psrp_realm          = "DOMAIN.COM"
  psrp_krb5_conf_path = "/etc/krb5.conf"
}
```

### Hyper-V Socket (PowerShell Direct)

```hcl
source "your-builder" "example" {
  communicator = "psrp"

  psrp_transport = "hvsock"
  psrp_vmid      = "12345678-1234-1234-1234-123456789012"
  psrp_username  = "Administrator"
  psrp_password  = "P@ssw0rd"
  psrp_auth_type = "ntlm"
}
```

## Development

```bash
make test          # Unit tests
make test-race     # With race detector
make fmt vet       # Format and vet
make build         # Compile check (example binary, not a usable plugin)
```

Acceptance tests require a real Windows target:

```bash
PACKER_ACC=1 make testacc
```

## Known Limitations

- **File transfer**: Uses base64 encoding inline in PowerShell scripts. Entire file is buffered in memory on both sides. Large files should be chunked (not yet implemented).
- **HvSocket testing**: Requires Windows host with Hyper-V. Cannot be tested on macOS/Linux.
- **Communicator interface**: `Upload`/`Download` don't accept context (SDK limitation), so they use a timeout-bounded context internally via `opContext()`.
- **Test coverage**: No unit tests yet. A mock-based test harness for `Start`, `Upload`, and `Download` is planned.

## License

Mozilla Public License Version 2.0 — see LICENSE file for details.

## Related

- [go-psrp](https://github.com/smnsjas/go-psrp) — Go PSRP protocol implementation
- [packer-plugin-sdk](https://github.com/hashicorp/packer-plugin-sdk) — Packer plugin development SDK
- [MS-PSRP Specification](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-psrp) — Protocol reference
