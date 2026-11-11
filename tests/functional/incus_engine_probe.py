#!/usr/bin/env python3
"""Run the dcache functional acceptance scenarios with **Incus** as the S3 backend.

This reuses tests/functional/run.py unchanged and only overrides setup(), so the
workload queries, hit counter, producer-removal proof and S3 fallback stages are
exactly the ones the repo already documents. What changes is the storage side:

  engine --PUT/GET presigned--> nginx (plain HTTP :9000, Host preserved)
                              --TLS--> Incus storage bucket on r730xd:8555

Run on the Linux host with Docker:
  python3 incus_engine_probe.py --output /data/dcache-dev/artifacts/incus-engine \
      --engine-image registry.dagger.io/engine:v1.0.0-beta.25 --scenario all
"""

import argparse
import os
import socket
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import run as H  # noqa: E402

# ---- Incus-backed S3 configuration (creds from env, never in the file) ----
INCUS_AK = os.environ["INCUS_S3_AK"]
INCUS_SK = os.environ["INCUS_S3_SK"]
INCUS_BUCKET = os.environ.get("INCUS_S3_BUCKET", "dagger-cache")
FRONT_DOOR_CONF = os.environ.get("FRONT_DOOR_CONF", "/data/dcache-dev/default.conf")
COORD_IMAGE = os.environ.get("COORD_IMAGE", "dcache:verify")

# The harness reads these module globals inside S3Client.
H.FIXTURE_S3_BUCKET = INCUS_BUCKET
H.FIXTURE_S3_USER = INCUS_AK
H.FIXTURE_S3_PASS = INCUS_SK

# S3Client's credentials are __init__ default arguments, bound at class
# definition time - patching the module globals above cannot reach them.
# Replace the class with a factory that always injects the Incus key pair.
_OrigS3Client = H.S3Client


class IncusS3Client(_OrigS3Client):
    def __init__(self, endpoint, key=None, secret=None, region="us-east-1"):
        super().__init__(endpoint, key=INCUS_AK, secret=INCUS_SK,
                         region="us-east-1")


H.S3Client = IncusS3Client


class IncusHarness(H.Harness):
    def wait_tcp(self, ip, port, timeout=60, what="tcp"):
        deadline = time.monotonic() + timeout
        while True:
            try:
                with socket.create_connection((ip, port), timeout=3):
                    return True
            except OSError:
                if time.monotonic() >= deadline:
                    raise TimeoutError("%s: %s:%d not reachable" % (what, ip, port))
                time.sleep(0.5)

    def setup(self):
        self.create_network()
        gateway = self.net_gateway_ip()
        self.evidence["host_gateway_ip"] = gateway

        # S3 front door. Registered under the "minio" suffix because
        # scenario_all() builds its SigV4 client from container_ip("minio"),
        # and object listing/deletion must go through the same origin.
        self.run_container(
            "minio", "nginxinc/nginx-unprivileged:1.27-alpine", [],
            volumes=[FRONT_DOOR_CONF + ":/etc/nginx/conf.d/default.conf:ro"])
        door_ip = self.container_ip("minio")
        self.wait_tcp(door_ip, 9000, timeout=60, what="s3 front door")
        self.evidence["s3_provider"] = "incus-storage-bucket"
        self.evidence["s3_front_door"] = (
            "nginx-unprivileged http://%s:9000 -> https://10.0.0.238:8555 "
            "(Host preserved, proxy_ssl_verify off)" % door_ip)
        self.evidence["s3_bucket"] = INCUS_BUCKET

        # Fail fast if the Incus bucket is not reachable/authenticated through
        # the proxy before burning engine time.
        probe = H.S3Client("http://%s:9000" % door_ip)
        probe.list()
        self.log("incus bucket reachable through front door")

        state_vol = self.prefix + "coord-state"
        self.docker.out(["volume", "create", state_vol])
        self.evidence.setdefault("volumes", []).append(state_vol)
        # distroless nonroot uid cannot write the fresh root-owned volume
        self.docker.out(["run", "--rm", "-v", state_vol + ":/state",
                         "busybox", "chown", "-R", "65532:65532", "/state"])

        coord_env = {
            "DCACHE_LISTEN": ":8080",
            "DCACHE_DB": "/state/cache.db",
            "DCACHE_POOL": "shared",
            "DCACHE_TOKEN": H.FIXTURE_TOKEN,
            "DCACHE_HMAC_SECRET": H.FIXTURE_HMAC,
            "S3_ENDPOINT": "http://%s:9000" % door_ip,
            "S3_PUBLIC_ENDPOINT": "http://%s:9000" % door_ip,
            "S3_BUCKET": INCUS_BUCKET,
            "S3_REGION": "us-east-1",
            "S3_ACCESS_KEY": INCUS_AK,
            "S3_SECRET_KEY": INCUS_SK,
        }
        self.run_container("coord", COORD_IMAGE, [], env=coord_env,
                           volumes=[state_vol + ":/state"])
        coord_ip = self.wait_coordinator()
        self.evidence["coord_ip"] = coord_ip

        port = self.hit.start()
        self.hit_url = "http://%s:%d/hit?job=shared" % (gateway, port)
        self.evidence["hit_url"] = self.hit_url
        self.evidence["hit_port"] = port

        try:
            img = self.docker.json(["image", "inspect", self.args.engine_image])
            self.evidence["engine_image_id"] = img[0].get("Id")
        except Exception as e:
            self.log("engine image inspect failed: %s" % e)


def main(argv=None):
    ap = argparse.ArgumentParser(description="dcache/Incus acceptance harness")
    ap.add_argument("--binary", default="unused-uses-image", help="unused")
    ap.add_argument("--output", required=True)
    ap.add_argument("--engine-image", required=True)
    ap.add_argument("--keep", action="store_true")
    ap.add_argument("--scenario", choices=["reuse", "all"], default="all")
    args = ap.parse_args(argv)
    return IncusHarness(args).run()


if __name__ == "__main__":
    sys.exit(main())
