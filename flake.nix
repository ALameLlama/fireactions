{
  description = "Fireactions packages and NixOS host module";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs, ... }:
    {
      packages = nixpkgs.lib.genAttrs [ "x86_64-linux" "aarch64-linux" ] (system:
        let
          pkgs = import nixpkgs { inherit system; };
          fireactions = pkgs.callPackage ./nix/package.nix {
            gitRevision = self.rev or self.dirtyRev or "unknown";
          };
        in
        {
          inherit fireactions;
          default = fireactions;
          tc-redirect-tap = pkgs.callPackage ./nix/tc-redirect-tap.nix { };
          guest-kernel = pkgs.callPackage ./nix/guest-kernel.nix { };
        });

      nixosModules = {
        fireactions = import ./nix/module.nix;
        default = self.nixosModules.fireactions;
      };
    };
}
