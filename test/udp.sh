# A published UDP port, carried through the tunnel (ADR 0038).
#
# Sourced by integration.sh rather than written inline, because it needs a
# probe built, a container run and a datagram round trip, and section 10 is
# long enough already.
#
# What it proves is one thing: a datagram sent to the port the user asked for
# on THIS machine reaches a container in the workspace and its answer comes
# back. Everything else here is setup for that.
udp_section() {
    echo
    echo "== 10b. a published UDP port answers here =="

    if ! (cd "$REPO/core" && CGO_ENABLED=0 GOOS=linux go build -o "$PROJECT/udpecho" ./probes/udpecho); then
        bad "could not build the udp echo probe"
        return
    fi

    if ! dockert run -d --name itest-udp -p 15353:5353/udp \
        -v "$PROJECT:/probe:ro" alpine:3 /probe/udpecho :5353 >"$WORK/udp-run.log" 2>&1; then
        bad "the udp echo container did not start: $(tail -2 "$WORK/udp-run.log" | tr '\n' ' ')"
        rm -f "$PROJECT/udpecho"
        return
    fi

    # The daemon publishes where it likes (ADR 0037), and the number below is
    # this machine's, so they must not be the same.
    published=$(dockert port itest-udp 5353/udp 2>/dev/null | head -1)
    case "$published" in
    *:15353) bad "the workspace bound 15353 itself" ;;
    *:[0-9]*) ok "the workspace published ${published##*:}/udp, not the 15353 asked for" ;;
    *) bad "could not read the workspace-side udp port: [$published]" ;;
    esac

    # The forward opens when the ports manager next reconciles, and the probe
    # has to be listening, so the datagram is retried rather than sent once.
    #
    # Sent with the same probe rather than netcat: the two netcats differ about
    # UDP and about -w, and a test that depends on which one a runner has is a
    # test that fails for a reason it is not about.
    answered=false
    for _ in $(seq 1 45); do
        reply=$("$PROJECT/udpecho" send 127.0.0.1:15353 "through the tunnel" 2>/dev/null)
        if [ "$reply" = "through the tunnel" ]; then
            answered=true
            break
        fi
        sleep 1
    done

    if [ "$answered" = true ]; then
        ok "a datagram reached the container and its answer came back"
    else
        bad "no answer came back from 127.0.0.1:15353"
        dockert logs itest-udp 2>&1 | sed 's/^/        probe: /' | tail -5
        sed 's/^/        /' "$WORK/up.log" | tail -8
    fi

    docker rm -f itest-udp >/dev/null 2>&1
    rm -f "$PROJECT/udpecho"
}
