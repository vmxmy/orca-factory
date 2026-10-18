# Clean machine for acceptance of `bin/orca-factory bootstrap` (OF-176): systemd as PID 1,
# one ordinary user with linger, and nothing but base tools. Run with
#   docker run -d --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock <image>
# and exec as `tester` with XDG_RUNTIME_DIR=/run/user/1000.
FROM debian:trixie
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus dbus-user-session libpam-systemd \
      git curl ca-certificates sudo procps \
 && rm -rf /var/lib/apt/lists/*
RUN useradd -m -u 1000 -s /bin/bash tester \
 && echo 'tester ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/tester \
 && mkdir -p /var/lib/systemd/linger && touch /var/lib/systemd/linger/tester
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
