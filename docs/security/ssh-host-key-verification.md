# SSH host key verification

StageZero connects to user-managed servers over SSH to deploy and inspect
containers. The server presents a host key during the SSH handshake; StageZero
must know in advance which key to expect, otherwise anyone able to intercept the
TCP connection can impersonate the server and read credentials or run commands.

StageZero verifies every SSH connection against an operator-managed OpenSSH
`known_hosts` file. An unknown host or a changed host key aborts the handshake
before authentication: StageZero never silently trusts a key it has not been
told to trust, and never learns trust from the first connection.

## Configuration

| Variable | Meaning |
| --- | --- |
| `SSH_KNOWN_HOSTS_FILE` | Absolute path to the trusted `known_hosts` file. Required for any SSH connection. |

The file is read fresh on each connection, so edits take effect without a
restart. It must be readable by the API process. When StageZero runs as more
than one replica, every replica must see the same file contents.

If `SSH_KNOWN_HOSTS_FILE` is unset, relative, or unreadable, StageZero refuses to
open any SSH connection rather than fall back to trusting the presented key.

## Initial trust (enrollment)

Enrollment is deliberate and out of band: the operator confirms the server's
host key by a means other than the SSH connection itself (provider console,
hosting dashboard, cloud metadata, or the machine's own console), then records
it.

1. On the server, read the host key fingerprint:

   ```bash
   ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
   ```

   The provider console or the machine itself is the source of truth. Compare
   this fingerprint to the value shown there.

2. Add the verified entries to `SSH_KNOWN_HOSTS_FILE`:

   ```bash
   ssh-keyscan -p 22 -t ed25519 example.internal >> /etc/stagezero/known_hosts
   ```

   `ssh-keyscan` collects the key over the network, so only use it after
   confirming it matches the fingerprint from step 1 — otherwise a
   man-in-the-middle could feed you its own key.

3. Confirm the reported key matches, then save. Connections to that host and
   port now succeed.

The file uses the standard OpenSSH format, including the `[host]:port` form for
non-default ports. If the server uses a custom SSH port, enroll that port.

## Host key rotation

Rotating a server's host key is an update to the trust store:

1. Generate the new host key on the server and note its fingerprint.
2. Verify the new fingerprint out of band.
3. Add the new entry to `SSH_KNOWN_HOSTS_FILE`. Both the old and new keys may be
   present at once, so the server keeps serving either during the switch.
4. Restart or switch the server to the new key. Connections keep working while
   either key is trusted.
5. Remove the old entry once all servers use the new key.

Replacing an entry in place is fine when rotation is atomic, but keeping both
entries avoids an outage if the switch is gradual.

## Failure behaviour

A host key that is missing or changed fails the connection with the
`host_key_untrusted` probe code. The server create flow shows it as an untrusted
host, not as a rejected SSH key: the fix is to verify and enroll the key, not to
change the SSH key used for authentication.
