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
          notmuch-mcp = pkgs.buildGo127Module {
            pname = "notmuch-mcp";
            version = version;
            src = self;

            # go-sum: a79eaf6b6b94e78587140a79559e1871b0123dc2cb2ecd4a466be08ef1f4fd41
            vendorHash = "sha256-UhWfSpKha7IenpqTjfXqMpgtA/sf2KcBFppA/7SHVfY=";

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
            go_1_27
            gopls
            golangci-lint
            just
            notmuch
          ];
        };
      }
    );
}
