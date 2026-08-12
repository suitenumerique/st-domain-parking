"""Create the certificate-storage bucket if it is missing.

Development helper only. A real deployment provisions its bucket out of band,
with a lifecycle policy and access controls this script has no business
inventing. Here it exists so `docker compose up` reaches a working stack
against the local RustFS without a manual step.
"""

import os
import sys
import time

import boto3
from botocore.exceptions import ClientError, EndpointConnectionError

ATTEMPTS = 30
DELAY = 2

# RustFS has just been started by compose, so expect to lose the first few.
ALREADY_OWNED = ("BucketAlreadyOwnedByYou", "BucketAlreadyExists")


def main() -> int:
    bucket = os.environ.get("S3_BUCKET", "")
    if not bucket:
        print("S3_BUCKET is not set", file=sys.stderr)
        return 2

    client = boto3.client(
        "s3",
        endpoint_url=os.environ.get("S3_ENDPOINT") or None,
        aws_access_key_id=os.environ.get("S3_ACCESS_KEY") or None,
        aws_secret_access_key=os.environ.get("S3_SECRET_KEY") or None,
        region_name=os.environ.get("S3_REGION", "us-east-1"),
    )

    for attempt in range(1, ATTEMPTS + 1):
        try:
            client.create_bucket(Bucket=bucket)
        except ClientError as exception:
            if exception.response["Error"]["Code"] in ALREADY_OWNED:
                print(f"bucket {bucket} already exists")
                return 0
            print(f"attempt {attempt}/{ATTEMPTS}: {exception}", file=sys.stderr)
        except EndpointConnectionError as exception:
            print(f"attempt {attempt}/{ATTEMPTS}: {exception}", file=sys.stderr)
        else:
            print(f"created bucket {bucket}")
            return 0

        time.sleep(DELAY)

    print(f"gave up creating bucket {bucket}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
