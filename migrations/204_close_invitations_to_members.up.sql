-- Migration 204: an invitation still live for an address that already has a
-- live member account is marked joined.
--
-- Signing up through an invitation used to set a password on an existing
-- account that had none (one that joined through Google or GitHub): whoever
-- held a live invitation link for a member's address could give that account
-- a password and sign in as them. Sign-up now answers 409 instead
-- (controllers/Auth EmailSignup), so such an invitation can never be used
-- for anything, and a live one is still a link someone could be holding.
-- They are closed, as joining would have closed them.
--
-- "Live" is models.Invitation.LiveAt's rule, written out here in SQL once: not
-- joined or expired, and its link not run out, where an invitation with no
-- expiry runs out a week after it was made. A live member is an account that
-- isn't deactivated, external (an import's row) or a bot. Addresses compare
-- without case, as every lookup does.
--
-- Idempotent: a second run finds nothing more to mark.
UPDATE invitations i
SET status = 'joined'
WHERE i.status NOT IN ('joined', 'expired')
  AND COALESCE(i.token_expires_at, i.created_at + interval '7 days') > NOW()
  AND EXISTS (
      SELECT 1 FROM users u
      WHERE LOWER(u.email_id) = LOWER(i.email)
        AND u.deleted_at IS NULL
        AND u.is_external = false
        AND u.is_bot = false
  );
