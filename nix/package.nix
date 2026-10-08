{ lib, buildGo126Module, gitRevision ? "unknown" }:

buildGo126Module (finalAttrs: {
  pname = "fireactions";
  version = (builtins.fromJSON (builtins.readFile ../.release-please-manifest.json)).".";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      (lib.fileset.fileFilter (file:
        file.hasExt "go" || file.name == "go.mod" || file.name == "go.sum"
      ) ../.)
      ../LICENSE
    ];
  };

  vendorHash = "sha256-IAFCtd66SWqiJFKFCeEbAqrXS04HBxXRL74y50pAAWs=";
  subPackages = [ "cmd/fireactions" ];
  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
    "-X github.com/ALameLlama/fireactions.Version=${finalAttrs.version}"
    "-X github.com/ALameLlama/fireactions.Commit=${gitRevision}"
  ];

  meta = {
    description = "Firecracker-based ephemeral CI runners and Forgejo plugin";
    homepage = "https://github.com/ALameLlama/fireactions";
    license = lib.licenses.asl20;
    mainProgram = "fireactions";
    platforms = [ "x86_64-linux" "aarch64-linux" ];
  };
})
