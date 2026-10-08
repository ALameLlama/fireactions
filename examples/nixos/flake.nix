{
  description = "NixOS host with Fireactions and an optional Forgejo Runner";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    fireactions = {
      url = "github:ALameLlama/fireactions";
      # For local changes: url = "path:/absolute/path/to/fireactions";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { nixpkgs, fireactions, ... }: {
    nixosConfigurations.fireactions-host = nixpkgs.lib.nixosSystem {
      # Use "aarch64-linux" for an ARM64 host and matching guest images.
      system = "x86_64-linux";
      modules = [
        # Keep your existing host configuration and its hardware imports.
        # Copy this flake beside configuration.nix, not into the source tree.
        ./configuration.nix
        fireactions.nixosModules.default
        {
          services.fireactions = {
            enable = true;
            # Existing, activated pool. This module does not create storage.
            devmapper.poolName = "containerd-thinpool";
            # Runtime string, not a Nix path or a file in the Nix store.
            imageArchive = "/var/lib/fireactions-images/ubuntu-24.04.tar";
            runner = {
              # Set false to manage Forgejo Runner separately.
              enable = true;
              url = "https://forgejo.example.org/";
              uuid = "REPLACE_WITH_RUNNER_REGISTRATION_UUID";
              # Prepare this root-owned 0600 file before the first switch.
              # Never put the token value in this flake or the repository.
              tokenFile = "/var/lib/fireactions-secrets/runner-token";
              capacity = 1;
              labels = [ "firecracker:firecracker://ubuntu-24.04" ];
            };
          };
        }
      ];
    };
  };
}
