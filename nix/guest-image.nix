{
  lib,
  stdenv,
  dockerTools,
  buildEnv,
  runCommand,
  writeText,
  jq,
  gnutar,
  sudo,
  nodejs_24,
  fireactions,
  name ? "localhost/fireactions-guest",
  ubuntuVersion ? "24.04",
  tag ? "ubuntu-${ubuntuVersion}",
  fromImage ? dockerTools.pullImage (
    {
      "20.04" = {
        x86_64-linux = {
          arch = "amd64";
          imageDigest = "sha256:02294ff31c0e1c3c064821cc840c480efc144a38d513274b1fd81a1dafe966fd";
          hash = "sha256-z1Rg298bK+TbbjFPK00vqGl03f4lycOGm5yzvmzhS20=";
        };
        aarch64-linux = {
          arch = "arm64";
          imageDigest = "sha256:9371d2f8f56f746573748d1310458e98f2406045891537cf3624ce39fcc9561f";
          hash = "sha256-IjOb8jvT72Pj80xeMXiNs8xkA87yvpBHEsiiI8HRvsI=";
        };
      };
      "22.04" = {
        x86_64-linux = {
          arch = "amd64";
          imageDigest = "sha256:f8a2134f0a5dccf54c8079c16cb3fa6e72205cc9471fb382f0952bdfa3b4219b";
          hash = "sha256-aB4oWe7u7yzuzgQSNPU8L4uaOkmkCYRQKciKCURZ4ns=";
        };
        aarch64-linux = {
          arch = "arm64";
          imageDigest = "sha256:ddc02a3497b00c2d09141cc6b5116abd3e5cdb6ef72081afbbe82825b404bf7b";
          hash = "sha256-vu5/dqe2dR+8AuBGBD4cA+wdtogekwDL1hyNjKwACHU=";
        };
      };
      "24.04" = {
        x86_64-linux = {
          arch = "amd64";
          imageDigest = "sha256:544aa495de873706d998cd2b158ff428002a1ef898c090dee57cd946217856c4";
          hash = "sha256-KiqKwQNx7WkZtvHpwC0ojF0A0btllt4/hbxNiJO+v70=";
        };
        aarch64-linux = {
          arch = "arm64";
          imageDigest = "sha256:302c6615e80fff5e1848ac91622b93ba2f6d2348d9791d39bd14d9fc6568a537";
          hash = "sha256-Av+1qIXWxuw1KA/7L6jOO1JqOThY4T7Nv0A4AmUMykg=";
        };
      };
    }
    .${ubuntuVersion}.${stdenv.hostPlatform.system}
    // {
      imageName = "ghcr.io/hostinger/fireactions-images/ubuntu${ubuntuVersion}";
      finalImageName = "ghcr.io/hostinger/fireactions-images/ubuntu${ubuntuVersion}";
      finalImageTag = ubuntuVersion;
    }
  ),
  dockerEnabled ? true,
  dockerSettings ? { },
  extraPackages ? [ ],
  extraCommands ? "",
  fakeRootCommands ? "",
}:

let
  runtime = buildEnv {
    name = "fireactions-guest-runtime";
    paths = [ nodejs_24 ] ++ extraPackages;
    pathsToLink = [ "/bin" ];
  };
  accounts = runCommand "fireactions-guest-accounts" {
    nativeBuildInputs = [ jq gnutar ];
    baseImage = fromImage;
    requireDocker = lib.boolToString dockerEnabled;
  } ''
    mkdir -p "$out" archive tools
    tar -xf "$baseImage" -C archive --no-same-owner
    jq -r '.[0].Layers[]' archive/manifest.json > layers
    while IFS= read -r layer; do
      tar -tf "archive/$layer" > entries
      files=()
      while IFS= read -r entry; do
        case "''${entry#./}" in
          etc/passwd|etc/group|etc/shadow|etc/gshadow) files+=("$entry") ;;
          lib/systemd/systemd|usr/lib/systemd/systemd) touch tools/systemd ;;
          usr/bin/sudo) touch tools/sudo ;;
          usr/bin/docker) touch tools/docker ;;
          usr/bin/dockerd) touch tools/dockerd ;;
          usr/bin/containerd) touch tools/containerd ;;
          usr/libexec/docker/cli-plugins/docker-buildx|usr/lib/docker/cli-plugins/docker-buildx) touch tools/buildx ;;
          usr/libexec/docker/cli-plugins/docker-compose|usr/lib/docker/cli-plugins/docker-compose) touch tools/compose ;;
        esac
      done < entries
      if (( ''${#files[@]} )); then
        tar -xf "archive/$layer" -C "$out" --no-same-owner --no-same-permissions "''${files[@]}"
      fi
    done < layers
    for file in passwd group shadow gshadow; do
      test -s "$out/etc/$file"
    done
    test -e tools/systemd
    test -e tools/sudo
    if [ "$requireDocker" = true ]; then
      for tool in docker dockerd containerd buildx compose; do
        test -e "tools/$tool"
      done
    fi
  '';
  path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";
in

dockerTools.buildLayeredImage {
  inherit name tag fromImage;
  compressor = "none";
  extraCommands = ''
    fireactions=${fireactions}
    runtime=${runtime}
    guestAccounts=${accounts}
    dockerEnabled=${lib.boolToString dockerEnabled}
    agentUnit=${../images/ubuntu-24.04/fireactions-agent.service}
    visudo=${sudo}/bin/visudo
    guestPath=${path}
    source ${./guest-image-setup.sh}
    ${lib.optionalString (dockerSettings != { }) ''
      mkdir -p etc/docker
      cp ${writeText "fireactions-docker-daemon.json" (builtins.toJSON dockerSettings)} etc/docker/daemon.json
    ''}
    ${extraCommands}
  '';
  fakeRootCommands = ''
    chown -R 0:0 .
    chown -R 1000:1000 home/ci workspace
    chmod 1777 tmp var/tmp
    chmod 0700 root
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
