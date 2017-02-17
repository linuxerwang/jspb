# jspb
jspb is a plugin for protocol buffers compiler which translates protocol
buffer messages into JavaScript (closure library) objects. The generated
closure objects are simply thin wrappers on JSON data.

# License

jspb uses the same 3-clause BSD license and keeps the original copyright
information from goprotobuf.

# Installation

go get github.com/linuxerwang/jspb/protoc-gen-jspb

# Uses

Examples can be found at the examples directory. The Makefile shows how to
run the compiler to generate jspb objects.
