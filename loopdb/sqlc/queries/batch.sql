-- name: GetUnconfirmedBatches :many
SELECT
        *
FROM
        sweep_batches
WHERE
        batch_state != 'confirmed';

-- name: UpsertBatch :exec
INSERT INTO sweep_batches (
        id,
        batch_state,
        batchTxid,
        batchPkScript,
        lastRbfHeight,
        lastRbfSatPerKw,
        maxTimeoutDistance
) VALUES (
        $1,
        $2,
        $3,
        $4,
        $5,
        $6,
        $7
) ON CONFLICT (id) DO UPDATE SET
        batch_state = $2,
        batchTxid = $3,
        batchPkScript = $4,
        lastRbfHeight = $5,
        lastRbfSatPerKw = $6;

-- name: ConfirmBatch :exec
UPDATE
        sweep_batches
SET
        batch_state = 'confirmed'
WHERE
        id = $1;

-- name: UpsertSweep :exec
INSERT INTO sweeps (
        swap_hash,
        batch_id,
        outpoint_txid,
        outpoint_index,
        amt
) VALUES (
        $1,
        $2,
        $3,
        $4,
        $5
) ON CONFLICT (swap_hash) DO UPDATE SET
        batch_id = $2,
        outpoint_txid = $3,
        outpoint_index = $4,
        amt = $5;


-- name: GetBatchSweeps :many
SELECT
        sweeps.*,
        swaps.*,
        loopout_swaps.*,
        htlc_keys.*
FROM
        sweeps
JOIN
        swaps ON sweeps.swap_hash = swaps.swap_hash
JOIN
        loopout_swaps ON sweeps.swap_hash = loopout_swaps.swap_hash
JOIN
        htlc_keys ON sweeps.swap_hash = htlc_keys.swap_hash
WHERE
        sweeps.batch_id = $1
ORDER BY
        sweeps.id ASC;
