# demo/Makefile -- build, smoke-test and publish the jailmachine demo images.
#
# Run this on a Mac with jailmachine started:
#
#     jm start
#     make -C demo build test
#     make -C demo push          # needs: podman login ghcr.io
#
# Kept separate from the repo Makefile on purpose: that one builds the jm
# binary and must stay runnable on a machine with no VM.

REGISTRY ?= ghcr.io/gabrielbelli
PODMAN   ?= jpodman
DATE_TAG ?= $(shell date -u +%Y%m%d)

# Native FreeBSD images. Built for the guest's own platform, so no --os flag.
# On an Apple Silicon Mac that means freebsd/arm64.
NATIVE := freebsd nginx-native volume

# Linux images. The guest defaults to freebsd/<arch>, so building or running
# these needs --os=linux; they execute through FreeBSD's Linuxulator.
LINUX  := linuxulator nginx-linuxulator

IMAGES := $(NATIVE) $(LINUX)

# Ports used by `make test` for the two server images.
PORT_NGINX_LINUX  ?= 18080
PORT_NGINX_NATIVE ?= 18081

.PHONY: help build test push clean login login-check $(addprefix build-,$(IMAGES)) $(addprefix push-,$(IMAGES))

help:
	@echo 'targets:'
	@echo '  build   build all $(words $(IMAGES)) demo images against the running machine'
	@echo '  test    run every image and check it behaves (starts and stops containers)'
	@echo '  push    push :latest and :$(DATE_TAG) to $(REGISTRY)'
	@echo '  clean   remove the local demo images and test leftovers'
	@echo
	@echo 'native (freebsd/<arch>): $(NATIVE)'
	@echo 'linux  (linux/<arch>)  : $(LINUX)'

# ---------------------------------------------------------------- build -----
build: $(addprefix build-,$(IMAGES))

$(addprefix build-,$(NATIVE)): build-%:
	$(PODMAN) build -t $(REGISTRY)/jm-demo-$*:latest -t $(REGISTRY)/jm-demo-$*:$(DATE_TAG) ./$*

$(addprefix build-,$(LINUX)): build-%:
	$(PODMAN) build --os=linux -t $(REGISTRY)/jm-demo-$*:latest -t $(REGISTRY)/jm-demo-$*:$(DATE_TAG) ./$*

# ----------------------------------------------------------------- test -----
# Every banner image must run with no arguments and exit 0. Every server image
# must answer /healthz with "ok" through a published port on the Mac.
# Output is captured into a variable rather than piped into `grep -q`:
# grep -q exits on the first match and SIGPIPEs podman mid-teardown, which
# leaves the --rm container (and its volume) behind.
test:
	@set -e; \
	echo '--- freebsd banner'; \
	out=$$($(PODMAN) run --rm $(REGISTRY)/jm-demo-freebsd:latest); \
	case "$$out" in *FreeBSD*) echo '    ok' ;; *) echo '    FAILED'; exit 1 ;; esac; \
	echo '--- linuxulator'; \
	out=$$($(PODMAN) run --rm --os=linux $(REGISTRY)/jm-demo-linuxulator:latest); \
	case "$$out" in *FreeBSD*) echo '    ok' ;; *) echo '    FAILED'; exit 1 ;; esac; \
	echo '--- volume'; \
	$(PODMAN) volume rm -f jm-demo-selftest >/dev/null 2>&1 || true; \
	out=$$($(PODMAN) run --rm -v jm-demo-selftest:/data $(REGISTRY)/jm-demo-volume:latest); \
	case "$$out" in *'now 1 line'*) : ;; *) echo '    FAILED (first run)'; exit 1 ;; esac; \
	out=$$($(PODMAN) run --rm -v jm-demo-selftest:/data $(REGISTRY)/jm-demo-volume:latest); \
	case "$$out" in *'now 2 lines'*) : ;; *) echo '    FAILED (data did not persist)'; exit 1 ;; esac; \
	$(PODMAN) volume rm -f jm-demo-selftest >/dev/null 2>&1 || true; \
	echo '    ok (data survived the container)'; \
	echo '--- nginx-linuxulator'; \
	$(MAKE) --no-print-directory serve-test \
	    NAME=jm-selftest-nginx-linux PORT=$(PORT_NGINX_LINUX) \
	    IMG=$(REGISTRY)/jm-demo-nginx-linuxulator:latest OSFLAG=--os=linux; \
	echo '--- nginx-native'; \
	$(MAKE) --no-print-directory serve-test \
	    NAME=jm-selftest-nginx-native PORT=$(PORT_NGINX_NATIVE) \
	    IMG=$(REGISTRY)/jm-demo-nginx-native:latest OSFLAG=; \
	echo; echo 'all demo images pass'

.PHONY: serve-test
serve-test:
	@$(PODMAN) rm -f $(NAME) >/dev/null 2>&1 || true; \
	$(PODMAN) run -d --name $(NAME) $(OSFLAG) -p $(PORT):80 $(IMG) >/dev/null; \
	ok=0; \
	for i in 1 2 3 4 5 6 7 8 9 10; do \
	    sleep 1; \
	    if [ "$$(curl -fsS -m 3 http://127.0.0.1:$(PORT)/healthz 2>/dev/null)" = 'ok' ]; then ok=1; break; fi; \
	done; \
	if [ $$ok -eq 1 ]; then \
	    curl -fsS -m 3 -o /dev/null http://127.0.0.1:$(PORT)/ && echo '    ok (/ and /healthz answered)'; \
	else \
	    echo '    FAILED -- logs follow:'; $(PODMAN) logs $(NAME) 2>&1 | tail -20; \
	    $(PODMAN) rm -f $(NAME) >/dev/null 2>&1; exit 1; \
	fi; \
	$(PODMAN) rm -f $(NAME) >/dev/null 2>&1

# ----------------------------------------------------------------- push -----
# NOTE ON ARCHITECTURE. `push` publishes whatever the running machine is,
# which on an Apple Silicon Mac is arm64. That is deliberate for the three
# NATIVE images: GitHub has no FreeBSD runner, and the freebsd-vm action used
# by .github/workflows/demo-images.yml is amd64, so freebsd/arm64 can only
# come from a maintainer's Mac. See the workflow header for the full story.
push: login-check $(addprefix push-,$(IMAGES))

login-check:
	@$(PODMAN) login --get-login ghcr.io >/dev/null 2>&1 || { \
	    echo 'not logged in. run:'; \
	    echo '  echo $$(gh auth token) | podman login ghcr.io -u <user> --password-stdin'; \
	    exit 1; }

$(addprefix push-,$(IMAGES)): push-%:
	$(PODMAN) push $(REGISTRY)/jm-demo-$*:latest
	$(PODMAN) push $(REGISTRY)/jm-demo-$*:$(DATE_TAG)

# ---------------------------------------------------------------- clean -----
clean:
	-@$(PODMAN) rm -f jm-selftest-nginx-linux jm-selftest-nginx-native >/dev/null 2>&1
	-@$(PODMAN) volume rm -f jm-demo-selftest >/dev/null 2>&1
	-@for i in $(IMAGES); do \
	    $(PODMAN) rmi -f $(REGISTRY)/jm-demo-$$i:latest $(REGISTRY)/jm-demo-$$i:$(DATE_TAG) >/dev/null 2>&1; \
	done; true
