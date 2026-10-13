"""Private COS transport. Executed with the pinned SDK virtual environment."""

import hashlib
import json
import os
from pathlib import Path
import re
import sys

from ops_common import OperationError, read_environment


def validate_bucket(client, bucket, prefix):
    acl = client.get_bucket_acl(Bucket=bucket)
    owner = acl.get("Owner", {}).get("ID")
    if not owner or any(g.get("Grantee", {}).get("ID") != owner for g in acl.get("Grant", [])):
        raise OperationError("backup bucket must grant access only through its owner's private IAM policy")
    rules = client.get_bucket_lifecycle(Bucket=bucket).get("Rule", [])
    enabled = [r for r in rules if r.get("Status") == "Enabled"]
    if len(enabled) != 1:
        raise OperationError("backup bucket requires one dedicated 14-day expiration rule")
    rule = enabled[0]
    if (rule.get("Filter", {}).get("Prefix", rule.get("Prefix")) != prefix
            or str(rule.get("Expiration", {}).get("Days")) != "14"
            or rule.get("Filter", {}).get("Tag") or rule.get("Transition")):
        raise OperationError("backup expiration must cover the entire backup prefix for 14 days")


def valid_filename(name):
    return bool(re.fullmatch(r"(?:mysql-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}\.sql\.gz\.enc(?:\.json)?|recovery-mysql-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}\.tar\.gz\.enc)", name))


def transfer(client, bucket, prefix, action, paths):
    results = []
    for path in paths:
        path = Path(path)
        if not valid_filename(path.name) or path.is_symlink():
            raise OperationError("COS transport accepts only owned encrypted backup files and their manifests")
        if action == "upload":
            with path.open("rb") as stream:
                client.put_object(Bucket=bucket, Key=prefix + path.name, Body=stream, EnableMD5=True,
                                  ServerSideEncryption="AES256", ContentType="application/octet-stream")
        elif path.exists():
            raise OperationError("download destination must be a new file")
        digest = hashlib.sha256()
        length = 0
        body = client.get_object(Bucket=bucket, Key=prefix + path.name)["Body"].get_raw_stream()
        output = None
        try:
            if action == "download":
                output = os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb")
            while True:
                block = body.read(1024 * 1024)
                if not block:
                    break
                length += len(block)
                if length > 64 * 1024 * 1024:
                    raise OperationError("encrypted backup exceeded the bounded transport size")
                digest.update(block)
                if output:
                    output.write(block)
        finally:
            body.close()
            if output:
                output.close()
        results.append({"filename": path.name, "sha256": digest.hexdigest(), "bytes": length})
    return results


def main():
    from importlib.metadata import version
    from qcloud_cos import CosConfig, CosS3Client
    from qcloud_cos.cos_exception import CosServiceError
    os.umask(0o077)
    try:
        if version("cos-python-sdk-v5") != "1.9.44":
            raise OperationError("the reviewed COS SDK version is required")
        request = json.load(sys.stdin)
        config_path = Path(request["config"])
        info = config_path.stat()
        if config_path.is_symlink() or info.st_mode & 0o077 or info.st_uid not in (0, os.getuid()):
            raise OperationError("COS backup credentials must be a private operator-owned file")
        config = read_environment(config_path)
        client = CosS3Client(CosConfig(Region=config["COS_REGION"], SecretId=config["COS_SECRET_ID"],
                                     SecretKey=config["COS_SECRET_KEY"], Scheme="https", Timeout=30))
        bucket, prefix = config["COS_BUCKET"], config["COS_PREFIX"]
        if not re.fullmatch(r"[a-z0-9-]+-[0-9]+", bucket) or prefix != "backups/":
            raise OperationError("a dedicated COS backup bucket and prefix are required")
        validate_bucket(client, bucket, prefix)
        if request["action"] not in ("upload", "download", "check"):
            raise OperationError("unknown COS backup action")
        result = [] if request["action"] == "check" else transfer(client, bucket, prefix, request["action"], request["paths"])
        print(json.dumps({"passed": True, "bucket": bucket, "objects": result}))
        return 0
    except CosServiceError as error:
        print(json.dumps({"passed": False, "status": error.get_status_code(), "code": error.get_error_code()}), file=sys.stderr)
    except Exception:
        print(json.dumps({"passed": False, "error": "COS backup request failed; private configuration or transport requires review"}), file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
