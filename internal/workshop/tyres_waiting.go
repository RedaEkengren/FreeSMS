package workshop

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// The tyre hotel's part of the action queue (#158): the customers a campaign
// wrote to who have not booked, and the sets nobody has come for. Both are
// counts read in one statement, with the shop's own days, and neither carries a
// name: the pages they open are the front desk's.
func tyresWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, nil
	}
	var follow, abandoned int
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			WITH s AS (SELECT coalesce((SELECT follow_up_days FROM tyre_change_settings ORDER BY changed_at DESC, id LIMIT 1), $1)::int AS follow,
			                  coalesce((SELECT abandon_days FROM tyre_change_settings ORDER BY changed_at DESC, id LIMIT 1), $2)::int AS lost)
			SELECT (SELECT count(*)::int FROM tyre_campaign_recipients r
			        JOIN tyre_campaigns c ON c.id = r.campaign_id, s
			        WHERE r.at <= clock_timestamp() - make_interval(days => s.follow)
			          AND EXISTS (SELECT 1 FROM tyre_sets ts JOIN tyre_set_placements pl ON pl.tyre_set_id = ts.id AND pl.removed_at IS NULL
			                      WHERE ts.customer_id = r.customer_id
			                        AND (ts.season = c.season OR (c.season = 'winter' AND ts.season IN ('winter_studded', 'winter_friction'))))
			          AND NOT EXISTS (SELECT 1 FROM tyre_sets t2 JOIN tyre_change_bookings tb ON tb.tyre_set_id = t2.id JOIN bookings b ON b.id = tb.booking_id
			                          WHERE t2.customer_id = r.customer_id AND b.status = 'booked'
			                            AND (t2.season = c.season OR (c.season = 'winter' AND t2.season IN ('winter_studded', 'winter_friction'))))),
			       (SELECT count(*)::int FROM tyre_sets ts
			        JOIN tyre_set_placements pl ON pl.tyre_set_id = ts.id AND pl.removed_at IS NULL
			        JOIN customers c ON c.id = ts.customer_id, s
			        CROSS JOIN LATERAL (SELECT greatest(pl.placed_at,
			            coalesce((SELECT max(x.charged_at) FROM tyre_storage_charges x WHERE x.tyre_set_id = ts.id), pl.placed_at),
			            coalesce((SELECT max(m.at) FROM tyre_job_moves m WHERE m.tyre_set_id = ts.id), pl.placed_at),
			            coalesce((SELECT max(tb.at) FROM tyre_change_bookings tb WHERE tb.tyre_set_id = ts.id), pl.placed_at)) AS last) q
			        WHERE q.last <= clock_timestamp() - make_interval(days => s.lost)
			          AND NOT EXISTS (SELECT 1 FROM shop_own_customers o WHERE o.customer_id = c.id))`,
			defaultFollowUpDays, defaultAbandonDays).Scan(&follow, &abandoned)
	})
	if err != nil {
		return nil, err
	}
	var out []ActionItem
	if follow > 0 {
		out = append(out, ActionItem{Kind: "tyre_follow_up", Module: access.Workshop, SubjectKind: "tyre_changes", Count: follow, Urgency: UrgencySoon})
	}
	if abandoned > 0 {
		out = append(out, ActionItem{Kind: "tyre_abandoned", Module: access.Workshop, SubjectKind: "tyre_abandoned", Count: abandoned, Urgency: UrgencySoon})
	}
	return out, nil
}
