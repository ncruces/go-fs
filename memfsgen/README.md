# A command to generate `memfs` http.FileSystems

```
Usage: memfsgen [options] <source-dir> <target-file>
  -mimetype value
        register a MIME type ("png:image/png", "txt:text/plain"…)
  -pkg string
        package name (default: lowercase name of <target-file> directory)
  -tag string
        build constraint
  -var string
        variable name (default "assets")
```

Install:
```
go install github.com/ncruces/go-fs/memfsgen
```

Typical usage will be through `go generate`:
```
//go:generate memfsgen -pkg main -var assets static assets.go
```

This generates a single `assets.go` file from the contents of directory `static`.

The file declares a single `var assets *memfs.FileSystem` in `package main`.
