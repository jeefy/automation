#!/usr/bin/env bash
# provision.sh — Create and bootstrap a .project repo for a CNCF project.
#
# Usage:
#   ./scripts/provision.sh --org <org> --name <name> [--repo <repo>] [options]
#   ./scripts/provision.sh --batch <file> [options]
#
# Options:
#   --org <org>           GitHub organization (e.g., "project-copacetic")
#   --name <name>         Project display name (e.g., "Copacetic")
#   --repo <repo>         Primary repo name (defaults to org name)
#   --batch <file>        Batch mode: read org|name|repo from file (pipe-delimited)
#   --dry-run             Print what would be done without making changes
#   --skip-secrets        Skip setting repository secrets
#   --skip-protection     Skip setting branch protection rules
#   --bootstrap-bin <p>   Path to bootstrap binary (default: ./bootstrap)
#   -h, --help            Show this help message
#
# Required environment variables:
#   GITHUB_TOKEN           GitHub token for gh CLI (set via gh auth login)
#   LANDSCAPE_REPO_TOKEN   Token for landscape repo PR creation
#
# Optional environment variables:
#   LFX_AUTH_TOKEN         LFX auth token for maintainer verification
#
# Batch file format (one project per line, # for comments):
#   org|name|repo
#   project-copacetic|Copacetic|copacetic
#   grpc|gRPC|grpc

set -euo pipefail

# Defaults
DRY_RUN=false
SKIP_SECRETS=false
SKIP_PROTECTION=false
BOOTSTRAP_BIN="./bootstrap"
BATCH_FILE=""
ORG=""
NAME=""
REPO=""

die() { echo "Error: $*" >&2; exit 1; }
info() { echo "==> $*" >&2; }
warn() { echo "WARNING: $*" >&2; }
dry() { if $DRY_RUN; then echo "[dry-run] $*" >&2; return 0; fi; return 1; }

usage() {
    sed -n '2,/^$/{ s/^# //; s/^#//; p }' "$0"
    exit 0
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        --org)          ORG="$2"; shift 2 ;;
        --name)         NAME="$2"; shift 2 ;;
        --repo)         REPO="$2"; shift 2 ;;
        --batch)        BATCH_FILE="$2"; shift 2 ;;
        --dry-run)      DRY_RUN=true; shift ;;
        --skip-secrets) SKIP_SECRETS=true; shift ;;
        --skip-protection) SKIP_PROTECTION=true; shift ;;
        --bootstrap-bin) BOOTSTRAP_BIN="$2"; shift 2 ;;
        -h|--help)      usage ;;
        *)              die "Unknown option: $1" ;;
    esac
done

# ──────────────────────────────────────────────
# Prerequisites
# ──────────────────────────────────────────────

check_prerequisites() {
    # gh CLI
    if ! command -v gh &>/dev/null; then
        die "gh CLI not found. Install from https://cli.github.com/"
    fi
    if ! gh auth status &>/dev/null; then
        die "gh CLI not authenticated. Run 'gh auth login' first."
    fi

    # Bootstrap binary
    if [[ ! -x "$BOOTSTRAP_BIN" ]]; then
        # Try building it
        if [[ -f "cmd/bootstrap/main.go" ]]; then
            info "Building bootstrap binary..."
            if ! dry "would build bootstrap binary"; then
                go build -o bootstrap ./cmd/bootstrap
                BOOTSTRAP_BIN="./bootstrap"
            fi
        else
            die "Bootstrap binary not found at '$BOOTSTRAP_BIN'. Build with: go build -o bootstrap ./cmd/bootstrap"
        fi
    fi

    # Required secrets for non-dry-run, non-skip-secrets
    if ! $DRY_RUN && ! $SKIP_SECRETS; then
        if [[ -z "${LANDSCAPE_REPO_TOKEN:-}" ]]; then
            die "LANDSCAPE_REPO_TOKEN environment variable is required (or use --skip-secrets)"
        fi
    fi
}

# ──────────────────────────────────────────────
# Provision a single project
# ──────────────────────────────────────────────

provision_project() {
    local org="$1"
    local name="$2"
    local repo="${3:-$org}"
    local target_repo="${org}/.project"

    info "Provisioning: ${target_repo} (name: ${name}, primary repo: ${repo})"

    # Step 1: Create repo if it doesn't exist
    if gh repo view "$target_repo" &>/dev/null; then
        info "  Repo ${target_repo} already exists, skipping creation"
    else
        if dry "would create repo: ${target_repo}"; then
            :
        else
            info "  Creating repo: ${target_repo}"
            gh repo create "$target_repo" \
                --public \
                --description "Project metadata for ${name} - CNCF .project automation" \
                || die "Failed to create repo ${target_repo}"
        fi
    fi

    # Step 2: Clone/init to temp directory
    local tmp_dir
    tmp_dir=$(mktemp -d)
    trap "rm -rf '$tmp_dir'" EXIT

    if dry "would clone ${target_repo} to ${tmp_dir}"; then
        :
    else
        info "  Cloning ${target_repo}..."
        if ! gh repo clone "$target_repo" "$tmp_dir" -- --depth=1 2>/dev/null; then
            # Empty repo - initialize manually
            info "  Empty repo detected, initializing..."
            git -C "$tmp_dir" init -b main
            git -C "$tmp_dir" remote add origin "https://github.com/${target_repo}.git"
        fi
    fi

    # Step 3: Run bootstrap
    if dry "would run bootstrap: ${BOOTSTRAP_BIN} -name '${name}' -github-org '${org}' -github-repo '${repo}' -output-dir '${tmp_dir}'"; then
        :
    else
        info "  Running bootstrap..."
        "$BOOTSTRAP_BIN" \
            -name "$name" \
            -github-org "$org" \
            -github-repo "$repo" \
            -output-dir "$tmp_dir" \
            || die "Bootstrap failed for ${name}"
    fi

    # Step 4: Commit and push
    if dry "would commit and push to ${target_repo}"; then
        :
    else
        info "  Committing and pushing..."
        git -C "$tmp_dir" add -A
        git -C "$tmp_dir" \
            -c user.name="cncf-automation[bot]" \
            -c user.email="projects@cncf.io" \
            commit -m "Initial .project scaffold for ${name}" \
            || { info "  Nothing to commit (already up to date)"; }
        git -C "$tmp_dir" push -u origin main \
            || die "Failed to push to ${target_repo}"
    fi

    # Step 5: Set secrets
    if ! $SKIP_SECRETS; then
        if dry "would set secrets on ${target_repo}"; then
            :
        else
            info "  Setting secrets..."
            echo "$LANDSCAPE_REPO_TOKEN" | gh secret set LANDSCAPE_REPO_TOKEN --repo "$target_repo"
            if [[ -n "${LFX_AUTH_TOKEN:-}" ]]; then
                echo "$LFX_AUTH_TOKEN" | gh secret set LFX_AUTH_TOKEN --repo "$target_repo"
            else
                warn "LFX_AUTH_TOKEN not set; skipping (maintainer verification won't work)"
            fi
        fi
    fi

    # Step 6: Branch protection
    if ! $SKIP_PROTECTION; then
        if dry "would set branch protection on ${target_repo}"; then
            :
        else
            info "  Setting branch protection..."
            gh api -X PUT "repos/${target_repo}/branches/main/protection" \
                --input - <<'PROTECTION' || warn "Branch protection failed (may require admin access)"
{
  "required_status_checks": {
    "strict": true,
    "contexts": ["validate-project", "validate-maintainers"]
  },
  "enforce_admins": false,
  "required_pull_request_reviews": {
    "required_approving_review_count": 1
  },
  "restrictions": null
}
PROTECTION
        fi
    fi

    # Step 7: Trigger validation workflow
    if dry "would trigger validation workflow on ${target_repo}"; then
        :
    else
        info "  Triggering validation workflow..."
        gh workflow run validate.yaml --repo "$target_repo" 2>/dev/null \
            || warn "Could not trigger workflow (may need a push event first)"
    fi

    info "  Done: https://github.com/${target_repo}"
    echo ""

    # Clean up trap for this iteration
    rm -rf "$tmp_dir"
    trap - EXIT
}

# ──────────────────────────────────────────────
# Main
# ──────────────────────────────────────────────

main() {
    check_prerequisites

    if [[ -n "$BATCH_FILE" ]]; then
        # Batch mode
        if [[ ! -f "$BATCH_FILE" ]]; then
            die "Batch file not found: ${BATCH_FILE}"
        fi

        local count=0
        local failed=0
        while IFS='|' read -r b_org b_name b_repo; do
            # Skip comments and empty lines
            [[ "$b_org" =~ ^[[:space:]]*# ]] && continue
            [[ -z "$b_org" ]] && continue

            # Trim whitespace
            b_org=$(echo "$b_org" | xargs)
            b_name=$(echo "$b_name" | xargs)
            b_repo=$(echo "${b_repo:-}" | xargs)

            if provision_project "$b_org" "$b_name" "$b_repo"; then
                ((count++))
            else
                warn "Failed to provision ${b_org}/${b_name}"
                ((failed++))
            fi
        done < "$BATCH_FILE"

        info "Batch complete: ${count} succeeded, ${failed} failed"
    else
        # Single mode
        [[ -z "$ORG" ]] && die "--org is required (or use --batch)"
        [[ -z "$NAME" ]] && die "--name is required (or use --batch)"

        provision_project "$ORG" "$NAME" "${REPO:-$ORG}"
    fi
}

main
