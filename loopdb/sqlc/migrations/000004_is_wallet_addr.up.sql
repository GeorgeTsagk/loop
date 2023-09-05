-- is_wallet_addr indicates whether the destination address of the swap is a 
-- wallet address.
ALTER TABLE loopout_swaps ADD is_wallet_addr BOOLEAN NOT NULL DEFAULT FALSE;