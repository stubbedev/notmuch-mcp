{
  description = "notmuch-mcp - MCP server for reading and tagging mail with notmuch";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        version = self.shortRev or self.dirtyShortRev or "dev";
      in
      {
        packages = {
          notmuch-mcp = pkgs.buildGoModule {
            pname = "notmuch-mcp";
            version = version;
            src = self;

            # go-sum: 597531960c802ee9b00db889e111b2b78aa2902f8c105587283ec9bc176858a0
            vendorHash = "sha256-6xlJLC20fALHvPnFWJ4wf5wc1bragXdMCWg6plD4x6I=";

            ldflags = [
              "-s"
              "-w"
              "-X main.Version=${version}"
              "-X main.Commit=${self.shortRev or self.dirtyShortRev or "dirty"}"
              "-X main.BuildDate=1970-01-01T00:00:00Z"
            ];

            # The server shells out to the system `notmuch` binary at runtime —
            # propagate it so `nix run` users get a working install.
            propagatedBuildInputs = [ pkgs.notmuch ];

            meta = {
              description = "MCP server for reading and tagging mail with notmuch";
              homepage = "https://github.com/stubbedev/notmuch-mcp";
              mainProgram = "notmuch-mcp";
            };
          };

          default = self.packages.${system}.notmuch-mcp;
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gopls
            golangci-lint
            just
            notmuch
          ];
        };
      }
    );
}
