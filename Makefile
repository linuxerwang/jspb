all:
	@echo "make demo: build examples."

demo:
	protoc -Iexamples --jspb_out=pkg_prefix=jspb:examples examples/*.proto
	protoc -Iexamples --jspb_out=pkg_prefix=jspb.examples:examples examples/depends/*.proto
