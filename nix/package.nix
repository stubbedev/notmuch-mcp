{ self, ... }:
{
  perSystem =
    {
      lib,
      pkgs,
      self',
      ...
    }:
    let
      version = self.shortRev or self.dirtyShortRev or "dev";
    in
    {
      packages = {
        notmuch-mcp = pkgs.buildGo127Module {
          pname = "notmuch-mcp";
          inherit version;
          src = lib.fileset.toSource {
            root = ../.;
            fileset = lib.fileset.difference (lib.fileset.gitTracked ../.) (
              lib.fileset.unions [
                ../flake.nix
                ../flake.lock
                ../nix
              ]
            );
          };

          # go-sum: dc72a53daffb3f1f7f970e9d8b76235e5eaaf36f2088b0950099a0255fd5b8ba
          vendorHash = "sha256-vA1Xrh7wAtniACEt29mM1wuh2eMt7yYH9laezqLiAhs=";

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

        default = self'.packages.notmuch-mcp;
      };
    };
}
