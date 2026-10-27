# Installed by juju-lens for the duration of a recording, in charms that run
# Python with PYTHONPATH pointing at the charm's venv directory (no .pth
# processing there). It loads the juju-lens tap, then the sitecustomize it
# shadows, if there is one further along sys.path.
try:
    import _juju_lens_tap  # noqa: F401
except Exception:
    pass


def _juju_lens_chain():
    import importlib.util
    import os
    import sys

    here = os.path.dirname(os.path.abspath(__file__))
    for d in sys.path:
        if not d or os.path.abspath(d) == here:
            continue
        path = os.path.join(d, "sitecustomize.py")
        if os.path.isfile(path):
            spec = importlib.util.spec_from_file_location("_juju_lens_shadowed_sitecustomize", path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            return


try:
    _juju_lens_chain()
except Exception:
    pass
