$resp = Invoke-RestMethod -Uri "https://api.github.com/repos/guenther-alka/cs-sync/actions/runs?per_page=1"
$resp.workflow_runs[0] | ForEach-Object { Write-Output "$($_.name) status=$($_.status) conclusion=$($_.conclusion)" }
