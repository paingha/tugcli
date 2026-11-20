"""A plugin for the tests that run main in a process of its own.

list prints to stdout, which main must send to stderr instead, and with the
account "slow" it sleeps until it is cancelled.
"""

import sys
import time

from tug_provider import Canceled, Cluster, Endpoints, Info, Provider, main, token


class Fixture(Provider):
    def info(self):
        return Info(name="fixture")

    def list(self, request):
        print("stray output from list")
        if request.account.id == "slow":
            try:
                time.sleep(60)
            except Canceled:
                print("cleaning up", file=sys.stderr)
                raise
        return [Cluster(id="c", name="c", endpoints=Endpoints(public="https://c.example"))]

    def credential(self, request):
        return token("t", None)


if __name__ == "__main__":
    sys.exit(main(Fixture()))
