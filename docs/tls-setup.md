# Management transport and TLS

`kiln-vmd` serves its API in exactly three supported management transport
modes. Pick one per host.

## Loopback HTTP

Loopback HTTP is the default. Use it when the CLI runs on the same host as
`kiln-vmd`:

```bash
KILN_VM_TLS_ENABLED=false
KILN_VM_LISTEN_ADDR=127.0.0.1:5456
KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=false
```

```bash
kiln-vm login --api-url http://127.0.0.1:5456 --token <token>
```

Loopback HTTP is not a remote-access mode. To manage the host from another
machine, use remote HTTPS. An SSH tunnel to the loopback port also works,
because the client still talks to a loopback address.

## Remote HTTPS

Remote HTTPS is the recommended mode for clients on other machines. `kiln-vmd`
gets its certificate from Let's Encrypt with the ACME HTTP-01 challenge:

```bash
KILN_VM_TLS_ENABLED=true
KILN_VM_TLS_DOMAIN=vms.example.com
KILN_VM_TLS_CERT_DIR=/var/lib/kiln-vm/certs
KILN_VM_TLS_LISTEN_ADDR=:443
KILN_VM_ACME_LISTEN_ADDR=:80
KILN_VM_ACME_EMAIL=you@example.com
KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=false
```

```bash
kiln-vm login --api-url https://vms.example.com --token <token>
```

TLS mode needs:

- A DNS record for `KILN_VM_TLS_DOMAIN` that points at the host.
- TCP ports 80 and 443 reachable from the internet.
- Nothing else listening on ports 80 or 443. `kiln-vmd` binds both itself and
  cannot share them with a web server or another ACME client. If the host
  already runs one, put `kiln-vmd` on loopback behind it instead.

TLS replaces the plaintext API listener. `kiln-vmd` does not serve HTTPS beside
`KILN_VM_LISTEN_ADDR`. The HTTPS listener accepts only requests whose SNI and
`Host` match `KILN_VM_TLS_DOMAIN`. The listener on port 80 answers HTTP-01
challenges, redirects GET and HEAD requests for the domain to HTTPS, and
rejects everything else with 400. It never serves the API in plaintext.

`kiln-vmd` keeps certificates in `KILN_VM_TLS_CERT_DIR` and reuses them after a
restart.

## Explicit insecure remote HTTP

Remote plaintext HTTP sends the bearer token in the clear. HTTP does not
provide application-layer encryption, so kiln-vm requires consent on both
sides before it will use it.

On the host, set a non-loopback listener and the server opt-in:

```bash
KILN_VM_TLS_ENABLED=false
KILN_VM_LISTEN_ADDR=0.0.0.0:5456
KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true
```

On each client, opt in for one saved URL:

```bash
kiln-vm login --api-url http://vms.internal:5456 --token <token> --allow-insecure-http
```

Or opt in for one process with both environment variables:

```bash
KILN_VM_API_URL=http://vms.internal:5456
KILN_VM_ALLOW_INSECURE_HTTP=true
```

The client setting cannot turn on the server listener, and the server setting
cannot make a client send its token over plaintext. Consent belongs to one URL.
Changing the saved or environment API URL does not carry consent over.

A LAN, VPN or WireGuard link limits who can reach the port and may encrypt
packets between peers. Plain HTTP over that link still has no
application-layer encryption and no HTTPS identity, so kiln-vm asks for the
same two-sided opt-in for every non-loopback address.

## Check the setup

1. Confirm that `kiln-vmd` starts with one of the three configurations. A
   configuration that mixes them exits with status 78.
2. For TLS, confirm that HTTP-01 reaches port 80 and that the domain serves a
   valid certificate on the TLS port.
3. Confirm that an authenticated request succeeds over HTTPS and that plain
   HTTP gets only a redirect or a 400.
4. Restart `kiln-vmd` and confirm that it reuses the cached certificate.
