// Package sync — the Daemon-side sync engine root. Sub-packages implement
// the Revision publish/recover protocol of
// docs/design/polyforge-vnext/05-sync-revision-recovery.md:
//
//   - revision: manifest construction, CAS block encoding, digests (05 §4)
//   - freeze:   write freeze + per-WI publish mutex + staging (05 §2.7)
//   - publish:  the six-step full-scan publish, pending records, GET-HEAD
//     disambiguation, final flush evidence (05 §6/§6.1/§2.8)
//   - watcher:  dirty-bit marking only, never publishes (05 §2.3)
//   - recover:  remote site reconstruction (05 §7.4)
package sync
