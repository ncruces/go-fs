# http.FileSystem implementations and utilities

This repo will house various `http.FileSystem` implementations and utilities.

For now, an in memory `http.FileSystem` implementation and static generator is provided.

## Why?

There are several alternative in memory `http.FileSystem` implementations, as well as many static file embedding tools.
Why invent this wheel again?

#### Requirements:

* load files at runtime (e.g. server startup), or embed them statically in the executable;
* load files directly from disk during development;
* multiple file systems in the same app;
* serve gzipped content efficiently;
* support caching, ranges, etc.

Package [`memfs`](https://godoc.org/github.com/ncruces/go-fs/memfs) and command [`memfsgen`](https://github.com/ncruces/go-fs/tree/master/memfsgen) implement this.
