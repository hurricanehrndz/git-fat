from typing import Protocol, List
import boto3
import os
from botocore.config import Config


class FatStores(Protocol):
    def sync(self, file_path: str) -> None:
        pass


class S3FatStore:
    def __init__(
        self,
        bucket_name: str,
        prefix: str = "",
        endpoint: str = "",
        access_key_id: str = "",
        secret_access_key: str = "",
    ):
        self.bucket_name = bucket_name
        self.prefix = prefix
        self.endpoint = endpoint
        self.id = access_key_id
        self.secret = secret_access_key

        self.s3 = self.get_s3_resource()
        self.bucket = self.s3.Bucket(self.bucket_name)

    def get_s3_resource(self):
        named_args = {}
        if self.endpoint:
            named_args["endpoint_url"] = self.endpoint

        if self.id and self.secret:
            named_args["aws_access_key_id"] = self.id
            named_args["aws_secret_access_key"] = self.secret

        return boto3.resource(
            "s3", config=Config(signature_version="s3v4"), verify=False, **named_args
        )

    def upload(self, local_filename: str, remote_filename=None) -> None:
        if remote_filename is None:
            remote_filename = os.path.basename(local_filename)
        self.bucket.upload_file(local_filename, remote_filename)

    def list(self) -> List:
        remote_files = [item.key for item in self.bucket.objects.all()]
        return remote_files

    def download(self, remote_filename: str, local_filename: str) -> None:
        self.bucket.download_file(remote_filename, local_filename)
