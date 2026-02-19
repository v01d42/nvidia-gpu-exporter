{
  description = "NVIDIA GPU Prometheus exporter";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { self, nixpkgs, ... }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
    in {
      packages.${system} = {
        default = pkgs.buildGoModule {
          pname = "nvidia-gpu-exporter";
          version = "0.2.5";

          src = ./.;

          # Run `nix build --no-link` and copy the hash from the error's `got:` line
          vendorHash = pkgs.lib.fakeHash;

          CGO_ENABLED = "1";

          nativeBuildInputs = with pkgs; [
            gcc
            pkg-config
          ];

          ldflags = [ "-w" "-s" ];

          meta = {
            description = "NVIDIA GPU Prometheus exporter using DCGM and NVML";
            homepage = "https://github.com/V01d42/nvidia-gpu-exporter";
            platforms = [ "x86_64-linux" ];
          };
        };
      };

      devShells.${system}.default = pkgs.mkShell {
        packages = with pkgs; [
          # Go toolchain (requires >= 1.25, use go_1_25 if available)
          go

          # C build tools for CGO
          gcc
          pkg-config

          # Go development tools
          gopls
          golangci-lint
          gotools
          delve

          # General tools
          git
          gh
        ];

        CGO_ENABLED = "1";

        shellHook = ''
          echo "nvidia-gpu-exporter dev environment"
          go version
        '';
      };
    };
}
