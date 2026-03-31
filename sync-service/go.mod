module github.com/liftedkilt/maven-central-sync/sync-service

go 1.23

require github.com/liftedkilt/maven-central-sync/internal/sync v0.0.0

replace github.com/liftedkilt/maven-central-sync/internal/sync => ../internal/sync
