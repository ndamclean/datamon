**Version: dev**

## datamon bundle diff

Diff a downloaded bundle with a remote bundle.

### Synopsis

Diff a downloaded bundle with a remote bundle.  --destination is a location previously passed to the `bundle download` command.

```
datamon bundle diff [flags]
```

### Options

```
      --bundle string            The hash id for the bundle, if not specified the latest bundle will be used
      --concurrency-factor int   Heuristic on the amount of concurrency used by various operations.  Turn this value down to use less memory, increase for faster operations. (default 100)
      --destination string       The path to the download dir
  -h, --help                     help for diff
      --label string             The human-readable name of a label
      --repo string              The name of this repository
```

### Options inherited from parent commands

```
      --context string   Set the context for datamon (default "dev")
      --upgrade          Upgrades the current version then carries on with the specified command
```

### SEE ALSO

* [datamon bundle](datamon_bundle.md)	 - Commands to manage bundles for a repo
