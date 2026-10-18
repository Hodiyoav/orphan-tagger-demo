"""Tiny proxy in front of moto that adds what moto lacks for this demo.

moto's Resource Groups Tagging API (TagResources) does not support IAM, which is the call the
orphan-tagger watcher makes. This shim answers TagResources for IAM role ARNs by calling IAM
TagRole on moto, and forwards every other request to moto untouched.

    clients  ->  :5000 (this shim)  ->  127.0.0.1:5001 (moto)
"""
import http.client
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import boto3

MOTO = ("127.0.0.1", 5001)
TAG_TARGET = "ResourceGroupsTaggingAPI_20170126.TagResources"
HOP_BY_HOP = {"connection", "transfer-encoding", "keep-alive", "content-length"}

iam = boto3.client("iam", endpoint_url="http://%s:%d" % MOTO, region_name="us-east-1",
                   aws_access_key_id="test", aws_secret_access_key="test")


class Shim(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if self.headers.get("X-Amz-Target") == TAG_TARGET:
            req = json.loads(body)
            arns = req["ResourceARNList"]
            if all(":iam::" in a and ":role/" in a for a in arns):
                for arn in arns:
                    iam.tag_role(RoleName=arn.rsplit("/", 1)[-1],
                                 Tags=[{"Key": k, "Value": v} for k, v in req["Tags"].items()])
                out = json.dumps({"FailedResourcesMap": {}}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/x-amz-json-1.1")
                self.send_header("Content-Length", str(len(out)))
                self.end_headers()
                self.wfile.write(out)
                return
        self.forward(body)

    do_GET = do_PUT = do_DELETE = do_HEAD = lambda self: self.forward(
        self.rfile.read(int(self.headers.get("Content-Length") or 0)))

    def forward(self, body):
        conn = http.client.HTTPConnection(*MOTO)
        conn.request(self.command, self.path, body, dict(self.headers))
        resp = conn.getresponse()
        data = resp.read()
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() not in HOP_BY_HOP:
                self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


ThreadingHTTPServer(("0.0.0.0", 5000), Shim).serve_forever()
