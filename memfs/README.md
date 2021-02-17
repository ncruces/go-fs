# An in memory fs.FS

[![GoDoc](https://godoc.org/github.com/ncruces/go-fs/memfs?status.svg)](https://godoc.org/github.com/ncruces/go-fs/memfs)

The filesystem can be [statically generated](https://github.com/ncruces/go-fs/tree/master/memfsgen),
or loaded (and modified) at runtime.
It is safe for concurrent reads (not writes), and biased towards read performance.

File names should be slash separated, and rooted (start with a slash).
Directories are implicit.
Files can be gzip-compressed in memory.
Methods are provided to serve gziped content directly to accepting HTTP clients.
