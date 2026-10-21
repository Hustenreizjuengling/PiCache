package prefill

// UnitFile is deploy/systemd/picache-prefill.service (a test keeps them
// equal): "picache prefill enable" installs it when no unit exists yet; an
// update from the web UI only replaces installed units.
const UnitFile = `# PiCache prefill: downloads the games chosen in the web UI (Cache ->
# Prefill) through the download cache. It holds the store sign-ins (a Steam
# refresh token), so it runs apart from PiCache: a process of its own under
# a user of its own (DynamicUser), without any capability, with no port in
# the network. PiCache's web UI talks to it over the Unix socket
# /run/picache-prefill/prefill.sock, which admits only PiCache's user and
# root. Off until enabled: "sudo picache prefill enable" (or
# "systemctl enable --now picache-prefill").
# Installed by deploy/install.sh and "picache prefill enable" to
# /usr/local/lib/systemd/system (the Debian package: /usr/lib/systemd/system).
# Local changes belong in a drop-in ("systemctl edit picache-prefill").
[Unit]
Description=PiCache prefill (games into the download cache)
Documentation=https://github.com/hustenreizjuengling/picache/blob/main/docs/DEPLOYMENT.md
Wants=network-online.target
After=network-online.target picache.service

[Service]
Type=simple
# A user that exists only while the service runs; the state directory
# /var/lib/private/picache-prefill (/var/lib/picache-prefill) belongs to it
# and is unreadable for every other user but root.
DynamicUser=yes
# The download cache, the socket and the state (PICACHE_PREFILL_*).
EnvironmentFile=-/etc/picache/prefill.env
ExecStart=/usr/local/bin/picache prefill
# Also after the service exits to load an updated binary.
Restart=always
RestartSec=5s

StateDirectory=picache-prefill
StateDirectoryMode=0700
RuntimeDirectory=picache-prefill
RuntimeDirectoryMode=0755
UMask=0077

# A background job: it yields to PiCache and everything else.
CPUWeight=50
IOWeight=50
Nice=10
MemoryMax=512M

CapabilityBoundingSet=
AmbientCapabilities=
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
ProcSubset=pid
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RemoveIPC=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
`

// UnitBinary is the binary UnitFile names (install.sh's path).
const UnitBinary = "/usr/local/bin/picache"
