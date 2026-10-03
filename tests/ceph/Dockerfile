FROM python:3.12-slim-bookworm
ARG S3TESTS_REF=5522d1c
RUN apt-get update && apt-get install -y --no-install-recommends git build-essential libssl-dev libffi-dev libxml2-dev libxslt1-dev && rm -rf /var/lib/apt/lists/*
RUN git clone https://github.com/ceph/s3-tests.git /suite && cd /suite && git checkout "$S3TESTS_REF" && git rev-parse HEAD > /suite-revision
WORKDIR /suite
RUN pip install --no-cache-dir -r requirements.txt pytest-timeout==2.4.0
COPY tests/ceph/run.py /runner.py
COPY tests/ceph/gateway_fixtures.py /gateway_fixtures.py
ENV S3_USE_SIGV4=1 AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true
ENTRYPOINT ["python", "/runner.py"]
CMD ["--full"]
