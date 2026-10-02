# The restricted-runner stand-in for hack/restricted-sim.sh.
#
# Deliberately a runner that COULD escalate if it were allowed to: the
# unprivileged user has a passwordless sudo rule. The simulation then runs
# the container with no-new-privileges, so the only thing standing between
# an action and root is the flag a Pod Security `restricted` pod sets.
# That is what makes a failure attributable: the control run (without the
# flag) shows the same image escalates fine.
FROM ubuntu:24.04
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      sudo git jq curl ca-certificates python3 \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 1001 --user-group --create-home --home-dir /home/runner --shell /bin/bash runner \
 && echo 'runner ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/runner \
 && chmod 0440 /etc/sudoers.d/runner
# Like the hosted images and unlike our runner image: sh is dash.
RUN readlink -f /bin/sh | grep -q dash
