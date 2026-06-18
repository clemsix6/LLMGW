-- Index reservation.expires_at so the self-healing prune in ReserveIfAdmitted
-- (DELETE FROM reservation WHERE project_id = $1 AND expires_at < now()) and the in-flight count's
-- expires_at filter stay cheap as reservations accumulate. IF NOT EXISTS keeps re-apply idempotent.
CREATE INDEX IF NOT EXISTS idx_reservation_expires_at ON reservation(expires_at);
