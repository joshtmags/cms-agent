# The ISO builder: Debian trixie amd64 with simple-cdd (Debian's tool for custom installer images).
# Built and run by iso/build-iso.sh; on an Apple Silicon Mac it runs emulated (linux/amd64).
FROM debian:trixie
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      simple-cdd debian-cd reprepro xorriso isolinux syslinux-common grub-efi-amd64-bin grub-pc-bin mtools dosfstools \
      curl ca-certificates gnupg gpg-agent minisign openssl lsb-release distro-info-data dose-distcheck \
 && rm -rf /var/lib/apt/lists/*
# simple-cdd 0.6.9 also fetches the i386 installer for amd64 images, which trixie no longer
# publishes (404). Skip it; trixie's debian-cd doesn't need it.
RUN grep -q 'if a == "amd64" and "i386" not in self.env.get("ARCHES"):' /usr/bin/build-simple-cdd \
 && sed -i 's/if a == "amd64" and "i386" not in self.env.get("ARCHES"):/if False:  # no i386 installer in trixie/' /usr/bin/build-simple-cdd
WORKDIR /build
