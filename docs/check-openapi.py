import os
import re
import sys

import yaml

path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "lumen-openapi.yaml")
raw = open(path, encoding="utf-8").read()
d = yaml.safe_load(raw)

print("openapi", d.get("openapi"), "| paths", len(d["paths"]))

ops = []
for p, item in d["paths"].items():
    for m, op in item.items():
        if m in ("get", "post", "put", "patch", "delete"):
            ops.append((p, m, op.get("operationId")))

schemas = d["components"]["schemas"]
print("operations", len(ops), "| schemas", len(schemas))

ids = [o[2] for o in ops]
dup = sorted({i for i in ids if ids.count(i) > 1})
print("dup operationId:", dup or "none")
missing_id = [o for o in ops if not o[2]]
print("missing operationId:", missing_id or "none")

refs = set(re.findall(r"\$ref:\s*'?#/components/(\w+)/([\w]+)", raw))
bad = [(k, n) for k, n in refs if n not in d["components"].get(k, {})]
print("broken refs:", bad or "none")

comps = d["components"]


def pname(p):
    if not isinstance(p, dict):
        return None
    if "$ref" in p:
        ref = p["$ref"].split("/")[-1]
        return comps.get("parameters", {}).get(ref, {}).get("name")
    return p.get("name")


prob = []
for p, item in d["paths"].items():
    want = re.findall(r"\{(\w+)\}", p)
    lvl = {pname(x) for x in item.get("parameters", []) if isinstance(x, dict)}
    for m, op in item.items():
        if m in ("get", "post", "put", "patch", "delete"):
            got = {pname(x) for x in op.get("parameters", []) if isinstance(x, dict)}
            miss = [x for x in want if x not in got | lvl]
            if miss:
                prob.append((p, m, miss))
print("missing path params:", prob or "none")

bad_int = re.findall(r"cpuCoresUsed:\s*\{\s*type:\s*integer", raw)
print("cpuCoresUsed 仍声明为 integer 的处数:", len(bad_int), "(应为 0)")

sys.exit(1 if (dup or bad or prob or missing_id) else 0)
