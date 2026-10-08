{
  description = "Fireactions packages and NixOS host module";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs, ... }:
    {
      packages = nixpkgs.lib.genAttrs [ "x86_64-linux" "aarch64-linux" ] (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          fireactions = pkgs.callPackage ./nix/package.nix {
            gitRevision = self.rev or self.dirtyRev or "unknown";
          };
          guest-image = pkgs.callPackage ./nix/guest-image.nix { inherit fireactions; };
        in
        {
          inherit fireactions guest-image;
          default = fireactions;
          tc-redirect-tap = pkgs.callPackage ./nix/tc-redirect-tap.nix { };
          guest-kernel = pkgs.callPackage ./nix/guest-kernel.nix { };
          "guest-image-ubuntu20.04" = guest-image.override { ubuntuVersion = "20.04"; };
          "guest-image-ubuntu22.04" = guest-image.override { ubuntuVersion = "22.04"; };
          "guest-image-ubuntu24.04" = guest-image.override { ubuntuVersion = "24.04"; };
        }
      );
      checks = nixpkgs.lib.genAttrs [ "x86_64-linux" "aarch64-linux" ] (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          storage = import ./nix/storage-test.nix { inherit pkgs; };
        }
      );

      nixosModules = {
        fireactions = import ./nix/module.nix;
        default = self.nixosModules.fireactions;
      };
    };
}
