all:
	@echo "make demo: build examples."

demo:
	protoc -Iexamples --js_out=pkg_prefix=jspb:examples examples/*.proto
	protoc -Iexamples --js_out=pkg_prefix=jspb.examples:examples examples/depends/*.proto
