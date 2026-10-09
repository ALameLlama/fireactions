test -x "$fireactions/bin/fireactions"

mkdir -p usr/bin usr/sbin usr/local/bin etc/tmpfiles.d etc/sysusers.d etc/pam.d \
  etc/systemd/system/multi-user.target.wants \
  etc/ssl/certs var/lib/dbus home/ci workspace root dev proc sys run tmp var/tmp
cp -a "$runtime/bin/." usr/bin/
ln -s "$systemd/lib/systemd/systemd" usr/sbin/init
ln -s "$fireactions/bin/fireactions" usr/local/bin/fireactions

# The Nix store binary cannot carry setuid permissions.
chmod u+w usr/bin
cp --remove-destination "$sudo/bin/sudo" usr/bin/sudo
cp "$pam/bin/unix_chkpwd" usr/sbin/unix_chkpwd
mkdir -p run/wrappers/bin
ln -s /usr/sbin/unix_chkpwd run/wrappers/bin/unix_chkpwd
# Nix PAM needs this helper path after the guest mounts an empty /run.
cat >etc/tmpfiles.d/fireactions-sudo.conf <<'EOF'
L+ /run/wrappers/bin/unix_chkpwd - - - - /usr/sbin/unix_chkpwd
EOF
cat >etc/sudoers <<EOF
Defaults secure_path="$guestPath"
root ALL=(ALL:ALL) ALL
ci ALL=(ALL:ALL) NOPASSWD: ALL
EOF
chmod 0440 etc/sudoers
"$sudo/bin/visudo" -cf etc/sudoers
cat >etc/pam.d/sudo <<EOF
auth required $pam/lib/security/pam_unix.so
account required $pam/lib/security/pam_unix.so
session required $pam/lib/security/pam_unix.so
EOF

# Nixpkgs keeps the stock units outside its runtime unit directory.
cp -a "$systemd/example/systemd/system/." etc/systemd/system/
cp -a "$systemd/example/tmpfiles.d/." etc/tmpfiles.d/
cp -a "$systemd/example/sysusers.d/." etc/sysusers.d/
chmod -R u+w etc/systemd/system etc/tmpfiles.d etc/sysusers.d
cp "$agentUnit" etc/systemd/system/fireactions-agent.service

ln -s /etc/systemd/system/fireactions-agent.service \
  etc/systemd/system/multi-user.target.wants/fireactions-agent.service
ln -sf /etc/systemd/system/multi-user.target etc/systemd/system/default.target

for unit in systemd-networkd.service systemd-networkd.socket \
  systemd-network-generator.service systemd-networkd-wait-online.service \
  systemd-resolved.service NetworkManager.service ssh.service sshd.service \
  getty@.service serial-getty@.service console-getty.service container-getty@.service getty.target; do
  ln -sf /dev/null "etc/systemd/system/$unit"
done

cat >etc/systemd/system.conf <<EOF
[Manager]
DefaultEnvironment=PATH=$guestPath
EOF

cat >etc/passwd <<'EOF'
root:x:0:0:root:/root:/bin/bash
ci:x:1000:1000:CI user:/home/ci:/bin/bash
nobody:x:65534:65534:Kernel Overflow User:/:/usr/bin/false
EOF

cat >etc/group <<'EOF'
root:x:0:
tty:x:3:
ci:x:1000:
nobody:x:65534:
EOF

cat >etc/shadow <<'EOF'
root:!:1:0:99999:7:::
ci:!:1:0:99999:7:::
nobody:!:1:0:99999:7:::
EOF

cat >etc/gshadow <<'EOF'
root:!::
tty:!::
ci:!::
nobody:!::
EOF
chmod 0600 etc/shadow etc/gshadow

cat >etc/nsswitch.conf <<'EOF'
passwd: files
group: files
shadow: files
hosts: files dns
networks: files
EOF

touch etc/machine-id etc/fstab
ln -s /etc/machine-id var/lib/dbus/machine-id
ln -s /proc/net/pnp etc/resolv.conf
ln -s "$cacert/etc/ssl/certs/ca-bundle.crt" etc/ssl/certs/ca-certificates.crt
ln -s ca-certificates.crt etc/ssl/certs/ca-bundle.crt
