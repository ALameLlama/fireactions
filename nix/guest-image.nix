{
  stdenv,
  dockerTools,
  buildEnv,
  systemd,
  bash,
  coreutils,
  git,
  curl,
  cacert,
  nodejs_24,
  sudo,
  pam,
  fireactions,
  name ? "localhost/fireactions-guest",
  tag ? "ubuntu-24.04",
  fromImage ? dockerTools.pullImage (
    {
      x86_64-linux = {
        arch = "amd64";
        imageDigest = "sha256:f610ab94648195aa356059f5b41d6085c9d4d903c072430cdd1af7bdb646106b";
        hash = "sha256-7Dtae54DRug93wFwTI/For7Mub1gZz0FGz/S31bBS3o=";
      };
      aarch64-linux = {
        arch = "arm64";
        imageDigest = "sha256:08571ca13e00ca07a2a84eab83a959b4242e22cceb16486a11bef1428c9e93a7";
        hash = "sha256-wLAQlPUS+b9AFwqoknPfILluklFcyYS8MOA8T5OvH2c=";
      };
    }
    .${stdenv.hostPlatform.system}
    // {
      imageName = "ubuntu";
      finalImageName = "ubuntu";
      finalImageTag = "24.04";
    }
  ),
  extraPackages ? [ ],
  extraCommands ? "",
  fakeRootCommands ? "",
}:

let
  runtime = buildEnv {
    name = "fireactions-guest-runtime";
    paths = [
      systemd
      bash
      coreutils
      git
      curl
      nodejs_24
      sudo
    ]
    ++ extraPackages;
    pathsToLink = [ "/bin" ];
  };
  path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";
in

dockerTools.buildLayeredImage {
  inherit name tag fromImage;
  compressor = "none";
  extraCommands = ''
    fireactions=${fireactions}
    runtime=${runtime}
    systemd=${systemd}
    sudo=${sudo}
    pam=${pam}
    agentUnit=${../images/ubuntu-24.04/fireactions-agent.service}
    cacert=${cacert}
    guestPath=${path}
    source ${./guest-image-setup.sh}
    ${extraCommands}
  '';
  fakeRootCommands = ''
    chown -R 0:0 .
    chown -R 1000:1000 home/ci workspace
    chmod 1777 tmp var/tmp
    chmod 0700 root
    chmod 4755 usr/bin/sudo usr/sbin/unix_chkpwd
    ${fakeRootCommands}
  '';
  config = {
    Cmd = [ "/sbin/init" ];
    StopSignal = "SIGRTMIN+3";
    WorkingDir = "/workspace";
    Env = [
      "PATH=${path}"
      "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"
      "SSL_CERT_DIR=/etc/ssl/certs"
    ];
  };
  meta.platforms = [
    "x86_64-linux"
    "aarch64-linux"
  ];
}