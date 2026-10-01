# code-runner — OneCamp's isolated execution sidecar.
#
# Two stages: build the static Go binary, then a slim Python runtime that ships
# a PINNED, curated data-analysis library set. No package installation happens
# at run time (no pip in the final image path the runner uses), so a run can't
# pull arbitrary dependencies. The container is meant to be deployed with the
# hardening the code-runner service carries in final-compose.yml (egress-less
# internal network, read-only rootfs, tmpfs
# /work, non-root, cap_drop=ALL, seccomp, pids_limit, gVisor recommended).

# ---- build the runner binary ----
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
# CGO off → a static binary that runs on the slim runtime image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/code-runner .

# ---- runtime: pinned python + data libs, non-root ----
FROM python:3.12-slim AS runtime

# Pinned, vetted libraries for data analysis. Pin exact versions for
# reproducibility + supply-chain control; upgrades are a deliberate image bump.
# --no-cache-dir keeps the image lean; pip is only used HERE at build time.
RUN pip install --no-cache-dir \
        numpy==2.1.3 \
        pandas==2.2.3 \
        matplotlib==3.9.2 \
    && rm -rf /root/.cache

# A fixed non-root user for every run.
RUN useradd --uid 10001 --create-home --shell /usr/sbin/nologin sandbox

COPY --from=build /out/code-runner /usr/local/bin/code-runner

# tmpfs work dir is mounted here by compose; create the mount point.
RUN mkdir -p /work && chown sandbox:sandbox /work

ENV CODE_RUNNER_WORKDIR=/work \
    CODE_RUNNER_ADDR=:9099 \
    MPLBACKEND=Agg

USER sandbox
EXPOSE 9099
ENTRYPOINT ["/usr/local/bin/code-runner"]
