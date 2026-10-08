{
  lib,
  buildGo126Module,
  fetchFromGitHub,
}:

buildGo126Module {
  pname = "tc-redirect-tap";
  version = "0.0.1";

  src = fetchFromGitHub {
    owner = "hostinger";
    repo = "tc-redirect-tap";
    rev = "6827f7b2f5e9ec675cb7dc9bbe4b201c228d565e";
    hash = "sha256-hMnZVFDT/ftD0+iSQqHLqz8gfTjuQv55Kr4mR2ranFI=";
  };

  # The release still declares Go 1.11, but its CNI plugins dependency requires
  # Go 1.20. Update the module graph so vendoring records dependency Go versions.
  postPatch = ''
    substituteInPlace go.mod \
      --replace-fail 'go 1.11' 'go 1.20' \
      --replace-fail 'github.com/onsi/ginkgo v1.16.5 // indirect' ""
    cat >> go.mod <<'EOF'

    require (
      github.com/davecgh/go-spew v1.1.1 // indirect
      github.com/hashicorp/errwrap v1.0.0 // indirect
      github.com/pmezard/go-difflib v1.0.0 // indirect
      github.com/vishvananda/netns v0.0.4 // indirect
      gopkg.in/yaml.v3 v3.0.1 // indirect
    )
    EOF
  '';

  vendorHash = "sha256-WMQALl82aFVZITNluL7DtWJ/3E/BGzWkGlAauY21/OM=";
  subPackages = [ "cmd/tc-redirect-tap" ];
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "CNI plugin connecting a tap device to a redirected network interface";
    homepage = "https://github.com/hostinger/tc-redirect-tap";
    license = lib.licenses.asl20;
    mainProgram = "tc-redirect-tap";
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
    ];
  };
}