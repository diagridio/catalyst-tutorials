#!/usr/bin/env python3
"""
tamper_history.py - flip one signed workflow-history event so Catalyst shows
the run as tampered. Two modes:

  DATABASE (default)  edit the Dapr state store DIRECTLY, behind Catalyst's
                      back. Proves that even a write straight to your own
                      Postgres gets caught, because the row no longer matches
                      the signature Catalyst recorded for it.

  ARCHIVE (--archive) edit an exported receipt (diagrid workflow archive
                      export) offline. No database needed. Proves the receipt
                      you'd hand an auditor can't be quietly altered either.

Either way it finds the first history event that still says FLAGGED and
rewrites it to CLEARED (same length, so the protobuf stays valid), as if the
flagged vendor had been waved through.

Database mode is a dry run by default - it shows the row it would change and
stops. Pass --apply to actually write.

  export PGHOST=... PGUSER=... PGDATABASE=postgres   # standard libpq vars
  read -s PGPASSWORD && export PGPASSWORD     # never hard-code this
  export INSTANCE_ID=inv-0312-run1               # only touch this instance's rows
  export CATALYST_PROJECT_ID=prj-123           # only if two projects share the db
  uv run --with 'psycopg[binary]' python tamper_history.py            # dry run
  uv run --with 'psycopg[binary]' python tamper_history.py --apply     # write it

  uv run python tamper_history.py --archive receipt.json --out receipt-tampered.json
"""
import base64
import json
import os
import sys

OLD, NEW = b"FLAGGED", b"CLEARED"


# --- archive mode: no database, no extra deps ---------------------------------

def tamper_archive(src, dst):
    with open(src) as f:
        archive = json.load(f)
    # Each history entry is the raw protobuf event, base64-encoded.
    for event in archive["history"]:
        raw = base64.b64decode(event["rawBase64"])
        if OLD not in raw:
            continue
        print(f"target event: history index {event['index']}")
        print(f"  contains {OLD.decode()} -> rewriting to {NEW.decode()}")
        event["rawBase64"] = base64.b64encode(raw.replace(OLD, NEW)).decode()
        with open(dst, "w") as f:
            json.dump(archive, f, indent=2)
        print(f"\nwrote tampered archive to {dst}.")
        print("now verify it:  diagrid workflow archive verify "
              f"{dst} --trust-anchor trust.pem")
        return
    sys.exit(f"no event contained {OLD.decode()} - already tampered, or wrong run.")


# --- database mode: edit the row in your own Postgres -------------------------

def tamper_database(apply):
    import psycopg  # uv run --with 'psycopg[binary]' ...
    from psycopg.types.json import Jsonb

    # Standard libpq variables - the same ones psql uses. Set them in your shell
    # (see the README, step 2). The host must be reachable over IPv4; on
    # Supabase that means the "Session pooler" host, not db.<ref>.supabase.co.
    missing = [v for v in ("PGHOST", "PGUSER", "PGPASSWORD") if not os.environ.get(v)]
    if missing:
        sys.exit(f"set {', '.join(missing)} in your shell first (see README step 2)")
    conn_info = dict(
        host=os.environ["PGHOST"],
        port=int(os.environ.get("PGPORT", "5432")),
        dbname=os.environ.get("PGDATABASE", "postgres"),
        user=os.environ["PGUSER"],
        password=os.environ["PGPASSWORD"],
        sslmode=os.environ.get("PGSSLMODE", "require"),
    )

    # Dapr's postgres (v1) store keeps one row per key in the `state` table,
    # value is jsonb; binary values are stored as a base64 JSON string.
    # Workflow history keys look like:  <app>||<instance>||history-00000N
    # Scope to one instance when INSTANCE_ID is set so an older run in the same
    # database is never touched by mistake.
    instance = os.environ.get("INSTANCE_ID")
    pattern = f"%||{instance}||history-%" if instance else "%||history-%"
    # Optional: the Catalyst project id (prj-NNN, printed by `diagrid dev run`
    # in the grpc URL) - needed only if two projects share this database AND
    # the same instance id.
    project = os.environ.get("CATALYST_PROJECT_ID")

    with psycopg.connect(**conn_info) as conn, conn.cursor() as cur:
        cur.execute(
            "SELECT key, value FROM state WHERE key LIKE %s ORDER BY key", (pattern,)
        )
        rows = cur.fetchall()
        if project:
            rows = [r for r in rows if f".{project}." in r[0]]
        if not rows:
            sys.exit(f"no history rows matched {pattern!r} - check the connection, "
                     "table name, INSTANCE_ID or CATALYST_PROJECT_ID")

        # Refuse to guess if more than one workflow (project/instance) matched.
        prefixes = sorted({k.rsplit("||", 1)[0] for k, _ in rows})
        if len(prefixes) > 1:
            print("more than one workflow matches - refusing to pick one:")
            for pfx in prefixes:
                print(f"  {pfx}")
            sys.exit("set INSTANCE_ID and/or CATALYST_PROJECT_ID=prj-<id> to narrow it down.")

        for key, value in rows:
            raw = base64.b64decode(value)
            if OLD not in raw:
                continue
            print(f"target row: {key}")
            print(f"  contains {OLD.decode()} -> rewriting to {NEW.decode()}")
            if not apply:
                print("\ndry run - nothing written. re-run with --apply to tamper.")
                return
            new_val = base64.b64encode(raw.replace(OLD, NEW)).decode()
            cur.execute("UPDATE state SET value = %s WHERE key = %s", (Jsonb(new_val), key))
            conn.commit()
            print(f"\ntampered {key} in the database.")
            print("now reload the execution in the Catalyst console to see it flagged.")
            return

        print(f"no row still contained {OLD.decode()} - already tampered, or wrong run.")


def main():
    args = sys.argv[1:]
    if "--archive" in args:
        src = args[args.index("--archive") + 1]
        dst = args[args.index("--out") + 1] if "--out" in args else "receipt-tampered.json"
        tamper_archive(src, dst)
    else:
        tamper_database(apply="--apply" in args)


if __name__ == "__main__":
    main()
