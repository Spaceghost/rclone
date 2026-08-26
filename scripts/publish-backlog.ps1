param(
    [string]$Repository = "Spaceghost/rclone-projection-vfs"
)

$ErrorActionPreference = "Stop"

gh auth status | Out-Null

$labelColors = @{
    "area:core" = "1D76DB"
    "area:backend" = "0E8A16"
    "area:http" = "5319E7"
    "area:s3" = "006B75"
    "area:cache" = "FBCA04"
    "area:desktop" = "D93F0B"
    "area:ci" = "BFD4F2"
    "priority:p0" = "B60205"
    "priority:p1" = "D93F0B"
    "priority:p2" = "FBCA04"
}
foreach ($entry in $labelColors.GetEnumerator()) {
    gh label create $entry.Key --repo $Repository --color $entry.Value --force | Out-Null
}

$issues = Get-Content -Raw "$PSScriptRoot/../backlog/issues.json" | ConvertFrom-Json
foreach ($issue in $issues) {
    $existing = gh issue list --repo $Repository --state all --search "in:title $($issue.title)" --json title | ConvertFrom-Json
    if ($existing.title -contains $issue.title) {
        Write-Host "Exists: $($issue.title)"
        continue
    }
    gh issue create --repo $Repository --title $issue.title --body $issue.body --label ($issue.labels -join ",") | Out-Null
    Write-Host "Created: $($issue.title)"
}
