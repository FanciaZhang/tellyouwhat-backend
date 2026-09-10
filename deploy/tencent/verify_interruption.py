"""Verify a manual interruption targets a failed, trusted, fully built production run."""

import json
import os
import re
import urllib.request


def validate(run, jobs, repository, sha, confirmation):
    if not re.fullmatch(r"[0-9a-f]{40}", sha) or confirmation != "interrupt:" + sha:
        raise ValueError("explicit confirmation must match the full deployment SHA")
    if (run.get("path") != ".github/workflows/backend.yml" or run.get("head_sha") != sha
            or run.get("head_branch") != "main" or run.get("event") not in ("push", "workflow_dispatch")
            or run.get("status") != "completed" or run.get("conclusion") != "failure"
            or run.get("repository", {}).get("full_name") != repository
            or run.get("head_repository", {}).get("full_name") != repository):
        raise ValueError("source must be a failed production Backend CI/CD run for the exact current main SHA")
    by_name = {job["name"]: job for job in jobs}
    for name in ["verify", *["container (" + s + ")" for s in ("gateway", "worker", "admin", "adminctl", "migrate", "maintenance")]]:
        job = by_name.get(name, {})
        if job.get("conclusion") != "success" or job.get("head_sha") != sha:
            raise ValueError("source verification and all six image builds must succeed for the exact SHA")
    failed = [job for job in jobs if job.get("name") == "deploy / deploy" and job.get("conclusion") == "failure"
              and job.get("head_sha") == sha and any(step.get("name") == "Deploy and verify internal services"
                  and step.get("conclusion") == "failure" for step in job.get("steps", []))]
    if len(failed) != 1:
        raise ValueError("source did not fail at the production activation step")


def main():
    repository, source_run = os.environ["GITHUB_REPOSITORY"], os.environ["SOURCE_RUN"]
    sha, confirmation = os.environ["EXPECTED_SHA"], os.environ["CONFIRMATION"]
    if (not re.fullmatch(r"[0-9]+", source_run) or sha != os.environ["GITHUB_SHA"]
            or os.environ["GITHUB_REF"] != "refs/heads/main"):
        raise ValueError("manual release must execute from the same main SHA as the failed source run")
    def get(path):
        request = urllib.request.Request("https://api.github.com/repos/" + repository + path,
            headers={"Authorization": "Bearer " + os.environ["GH_TOKEN"], "Accept": "application/vnd.github+json",
                     "X-GitHub-Api-Version": "2022-11-28"})
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)
    run = get("/actions/runs/" + source_run)
    jobs, page = [], 1
    while True:
        result = get("/actions/runs/" + source_run + "/attempts/" + str(run["run_attempt"]) + "/jobs?per_page=100&page=" + str(page))
        jobs.extend(result["jobs"])
        if len(jobs) >= result["total_count"]:
            break
        page += 1
    validate(run, jobs, repository, sha, confirmation)
    # Branch movement invalidates approval; no deployment of an older verified tree.
    if get("/git/ref/heads/main")["object"]["sha"] != sha:
        raise ValueError("main moved; obtain new verification and a new capacity refusal")
    print("Verified explicit interruption selection. Server capacity refusal must still match before any stop.")


if __name__ == "__main__":
    main()
