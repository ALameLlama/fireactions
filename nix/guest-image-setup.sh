test -x "$fireactions/bin/fireactions"

mkdir -p usr/bin usr/sbin usr/local/bin etc/tmpfiles.d etc/systemd/system/multi-user.target.wants \
  etc/ssl/certs var/lib/dbus home/ci workspace opt/hostedtoolcache root dev proc sys run tmp var/tmp

# Set ownership at boot because the base image can contain existing tool cache entries.
cat >etc/tmpfiles.d/fireactions-tool-cache.conf <<'EOF'
Z /opt/hostedtoolcache - ci ci -
EOF

cp -a "$runtime/bin/." usr/local/bin/
chmod u+w usr/local/bin
ln -sf /lib/systemd/systemd usr/sbin/init
ln -sf "$fireactions/bin/fireactions" usr/local/bin/fireactions
ln -sf /usr/local/bin/fireactions usr/bin/fireactions
cp "$agentUnit" etc/systemd/system/fireactions-agent.service
ln -sf /etc/systemd/system/fireactions-agent.service \
  etc/systemd/system/multi-user.target.wants/fireactions-agent.service
ln -sf /lib/systemd/system/multi-user.target etc/systemd/system/default.target

cat >etc/sudoers <<EOF
Defaults secure_path="$guestPath"
root ALL=(ALL:ALL) ALL
ci ALL=(ALL:ALL) NOPASSWD: ALL
EOF
chmod 0440 etc/sudoers
"$visudo" -cf etc/sudoers

cp -a "$guestAccounts/etc/." etc/
chmod u+w etc etc/passwd etc/group etc/shadow etc/gshadow
awk -F: 'BEGIN { OFS = ":" } $1 != "ci" && $3 != 1000 { print }
  END { print "ci:x:1000:1000:CI user:/home/ci:/bin/bash" }' etc/passwd > etc/passwd.new
mv etc/passwd.new etc/passwd
awk -F: 'BEGIN { OFS = ":" } $1 != "ci" && $3 != 1000 {
    if ($1 == "docker" && ("," $4 ",") !~ /,ci,/) $4 = $4 == "" ? "ci" : $4 ",ci"
    print
  } END { print "ci:x:1000:" }' etc/group > etc/group.new
mv etc/group.new etc/group
awk -F: 'BEGIN { OFS = ":" } NR == FNR { users[$1] = 1; next }
  $1 in users && $1 != "ci" { $2 = "!"; print }
  END { print "ci:!:1:0:99999:7:::" }' etc/passwd etc/shadow > etc/shadow.new
mv etc/shadow.new etc/shadow
awk -F: 'BEGIN { OFS = ":" } NR == FNR { groups[$1] = 1; next }
  $1 in groups && $1 != "ci" {
    $2 = "!"
    if ($1 == "docker" && ("," $4 ",") !~ /,ci,/) $4 = $4 == "" ? "ci" : $4 ",ci"
    print
  } END { print "ci:!::" }' etc/group etc/gshadow > etc/gshadow.new
mv etc/gshadow.new etc/gshadow
chmod 0600 etc/shadow etc/gshadow

test "$(awk -F: '$1 == "ci" { print $3 ":" $4 }' etc/passwd)" = "1000:1000"
awk -F: '$2 != "!" { exit 1 }' etc/shadow
if [ "$dockerEnabled" = true ]; then
  awk -F: '$1 == "docker" && ("," $4 ",") ~ /,ci,/ { found = 1 } END { exit !found }' etc/group
  mkdir -p etc/systemd/system/fireactions-agent.service.d
  cat >etc/systemd/system/fireactions-agent.service.d/docker.conf <<'EOF'
[Unit]
Requires=docker.service
After=docker.service
EOF
  ln -sf /lib/systemd/system/docker.service etc/systemd/system/multi-user.target.wants/docker.service
else
  for unit in docker.service docker.socket containerd.service; do
    ln -sf /dev/null "etc/systemd/system/$unit"
  done
fi

for unit in systemd-networkd.service systemd-networkd.socket \
  systemd-network-generator.service systemd-networkd-wait-online.service \
  systemd-resolved.service NetworkManager.service ssh.service ssh.socket sshd.service \
  getty@.service serial-getty@.service console-getty.service container-getty@.service getty.target; do
  ln -sf /dev/null "etc/systemd/system/$unit"
done

# Whiteouts remove files from the base layers, not only from this overlay.
mkdir -p etc var/lib root home/ci opt
for entry in etc/netplan etc/systemd/network etc/ssh var/lib/ssh root/.ssh home/ci/.ssh opt/runner; do
  mkdir -p "$(dirname "$entry")"
  touch "$(dirname "$entry")/.wh.$(basename "$entry")"
done

cat >etc/systemd/system.conf <<EOF
[Manager]
DefaultEnvironment=PATH=$guestPath
EOF

cat >etc/nsswitch.conf <<'EOF'
passwd: files
group: files
shadow: files
hosts: files dns
networks: files
EOF

: > etc/machine-id
touch etc/fstab
ln -sf /etc/machine-id var/lib/dbus/machine-id
ln -sf /proc/net/pnp etc/resolv.conf
