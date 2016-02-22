/*
 * The main function for protoc-gen-js. It's cloned from htts://github.com/golang/protobuf
 * then modified for protoc-gen-js.
 *
 * jspb uses the same 3-clause BSD license and keeps the original copyright
 * information from goprotobuf.
 */
package main

import (
	"io/ioutil"
	"os"

	"github.com/golang/protobuf/proto"
	"github.com/linuxerwang/jspb/protoc-gen-jspb/generator"
)

func main() {
	g := generator.New()

	data, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		g.Error(err, "reading input")
	}

	// Parse the request.
	if err := proto.Unmarshal(data, g.Request); err != nil {
		g.Error(err, "parsing input proto")
	}

	if len(g.Request.FileToGenerate) == 0 {
		g.Fail("no files to generate")
	}

	g.CommandLineParameters(g.Request.GetParameter())

	// Create a wrapped version of the Descriptors and EnumDescriptors that
	// point to the file that defines them.
	g.WrapTypes()

	g.SetPackageNames()
	g.BuildTypeNameMap()

	g.GenerateAllFiles()

	// Send back the results.
	data, err = proto.Marshal(g.Response)
	if err != nil {
		g.Error(err, "failed to marshal output proto")
	}
	_, err = os.Stdout.Write(data)
	if err != nil {
		g.Error(err, "failed to write output proto")
	}
}
