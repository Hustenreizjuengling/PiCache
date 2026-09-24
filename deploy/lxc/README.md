# PiCache in a Proxmox LXC container

An unprivileged Debian container is the recommended way to run PiCache on
Proxmox VE. PiCache is installed natively with `deploy/install.sh`, exactly
as on bare metal. Docker inside LXC works less reliably and is not
recommended.

The examples use container ID `120`, the container address `192.168.1.5` and a
NAS at `192.168.1.10`. Replace them with your values.

## 1. Create the container

Download a Debian 12 or 13 template (**Datacenter → node → local → CT
Templates → Templates**, or on the host):

```sh
pveam update
pveam available --section system | grep debian
pveam download local debian-13-standard_<version>_amd64.tar.zst
```

Create an **unprivileged** container with nesting enabled (nesting lets
systemd set up the sandbox of the PiCache service) and a **static** address,
since this container becomes your network's DNS server:

```sh
pct create 120 local:vztmpl/debian-13-standard_<version>_amd64.tar.zst \
  --hostname picache --unprivileged 1 --features nesting=1 \
  --cores 2 --memory 2048 --swap 512 --rootfs local-lvm:8 \
  --net0 name=eth0,bridge=vmbr0,ip=192.168.1.5/24,gw=192.168.1.1 \
  --onboot 1
```

1–2 GB of memory is enough. The root disk holds the databases (the query log
database is capped at 2 GiB by default), so 8 GB is plenty. Put the download
cache on its own volume (step 3), not on the root disk.

## 2. Install PiCache

Copy a release binary for the host architecture and the `deploy/` directory
of the same version into the container, then run the installer inside it:

```sh
# on the Proxmox host, in the PiCache source tree
tar -czf /tmp/picache-deploy.tar.gz deploy
pct start 120
pct push 120 /tmp/picache-deploy.tar.gz /root/picache-deploy.tar.gz
pct push 120 bin/picache-linux-amd64 /root/picache

pct enter 120
cd /root && tar -xzf picache-deploy.tar.gz
sh deploy/install.sh --binary /root/picache
```

The installer creates the `picache` user, the systemd unit and
`/etc/picache/picache.env`, checks for port-53 conflicts and starts the
service. Then open `http://192.168.1.5:8080/` and enter the one-time setup
token:

```sh
pct exec 120 -- picache setup-token
```

Do not use `--with-host-apply` in an unprivileged container. The installer
skips it there, because the helper cannot mount anything (see step 4).

## 3. Local cache disk (optional)

To keep the cache off the root disk, give the container a dedicated volume
(500 GB here) at the built-in cache directory. Stop the container first, and
exclude the volume from backups:

```sh
pct stop 120
pct set 120 -mp0 local-lvm:500,mp=/var/cache/picache,backup=0
pct start 120
```

systemd gives the directory to the `picache` user when the service starts. A
cache is rebuildable data and does not need to be backed up.

## 4. NAS cache storage

### Why the container cannot mount the share itself

An unprivileged container runs in its own user namespace. The kernel allows
CIFS/SMB and NFS mounts only with `CAP_SYS_ADMIN` in the *initial* user
namespace, which a container never has. Proxmox's `features: mount=nfs;cifs`
only relaxes the AppArmor profile. It helps only privileged containers, where
container root is host root and a hanging NFS server can block the host. So
PiCache's host-apply helper cannot work here. The Proxmox host mounts the
share instead and passes it into the container as a bind mount point.

### UID mapping

Files in an unprivileged container belong to shifted IDs on the host: host
UID = offset + container UID. The default offset is 100000:

```sh
pct exec 120 -- cat /proc/self/uid_map   # "0 100000 65536" → offset 100000
pct exec 120 -- id picache               # e.g. uid=999(picache) gid=999(picache)
```

With these values the share must be writable for host UID/GID
`100999:100999`. The web UI shows the computed values in the snippets of a
storage target (**Cache → Storage**).

### Mount the share on the Proxmox host

Use the NAS's IP address, not its name. At boot the host may mount the share
before PiCache, your DNS server, is running. The commands below are examples.
Once you have added the target in PiCache (last step), its snippets show the
same commands with your server, share and IDs filled in.

SMB (install `cifs-utils` on the host):

```sh
apt install cifs-utils
install -d -m 0700 /etc/picache-nas
cat > /etc/picache-nas/credentials <<'EOF'
username=picache
password=<your NAS password>
EOF
chmod 0600 /etc/picache-nas/credentials
mkdir -p /mnt/picache-nas
```

`/etc/fstab` on the host:

```
//192.168.1.10/picache /mnt/picache-nas cifs credentials=/etc/picache-nas/credentials,vers=3.1.1,uid=100999,gid=100999,file_mode=0640,dir_mode=0750,soft,nosuid,nodev,noexec,noatime,_netdev,nofail 0 0
```

NFS (install `nfs-common` on the host). On the NAS, either give the export to
`100999:100999`, or export it with `all_squash,anonuid=100999,anongid=100999`,
and allow only the Proxmox host's address:

```
192.168.1.10:/volume1/picache /mnt/picache-nas nfs4 vers=4.2,proto=tcp,softerr,timeo=100,retrans=2,nosuid,nodev,noexec,noatime,_netdev,nofail 0 0
```

Then run `mount /mnt/picache-nas` and check that it is mounted with
`findmnt /mnt/picache-nas`.

### Pass it into the container

```sh
pct set 120 -mp1 /mnt/picache-nas,mp=/srv/picache/nas
pct reboot 120
```

Bind mount points are not included in `vzdump` backups, which is what you
want for a cache. If the share was not mounted when the container started,
it may not appear inside the container. Restart the container after mounting
it.

### Use it in PiCache

In **Cache → Storage**, add an SMB or NFS target with mode *external* and
path `/srv/picache/nas`. Run the test, initialise the store and activate it.
PiCache checks that the path is a mount of the expected type and holds its
store marker. If the NAS is missing, the target goes offline and downloads
pass through uncached. PiCache never writes into the empty directory. DNS is
never affected.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `picache.service` fails with `status=226/NAMESPACE` | Enable nesting: `pct set 120 --features nesting=1`, then restart the container. |
| `port 53 is in use` | Another DNS service runs in the container. The installer prints the fix; see [docs/DEPLOYMENT.md](../../docs/DEPLOYMENT.md#port-53-conflicts). |
| `permission denied` on `/srv/picache/nas` | Host UID/GID in the fstab options (or NAS export ownership) does not match 100000 + the container's `picache` UID/GID. |
| The NAS target stays offline | `findmnt /mnt/picache-nas` on the host, then restart the container. **Cache → Storage** shows the reason. |
