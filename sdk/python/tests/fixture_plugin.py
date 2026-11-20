"""A plugin for the tests that run main in a process of its own.

list prints to stdout, which main must send to stderr instead. With the
account "slow" it waits, with the SDK's sleep, until it is cancelled; with
"stubborn" it waits with time.sleep, which nothing interrupts in serve
mode.
"""

import sys
import time

from tug_provider import Canceled, Cluster, Endpoints, Info, Provider, main, sleep, token


class Fixture(Provider):
    def info(self):
        return Info(name="fixture")

    def list(self, request):
        print("stray output from list")
        if request.account.id == "slow":
            try:
                sleep(60)
            except Canceled:
                print("cleaning up", file=sys.stderr)
                raise
        if request.account.id == "stubborn":
            time.sleep(60)
        return [Cluster(id="c", name="c", endpoints=Endpoints(public="https://c.example"))]

    def credential(self, request):
        return token("t", None)


if __name__ == "__main__":
    sys.exit(main(Fixture()))
