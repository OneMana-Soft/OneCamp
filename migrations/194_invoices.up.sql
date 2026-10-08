-- Migration 194: invoices a project's admins save from its billable time,
-- numbered, with their lines as they were billed and a state (draft, sent,
-- paid, void), so what's owed and what's late can be seen. Money is in
-- hundredths of the currency's unit, as project_billing keeps rates. See
-- business/Invoice.
CREATE TABLE IF NOT EXISTS invoices (
    "id"             uuid PRIMARY KEY,
    "project_id"     uuid NOT NULL,
    "number"         varchar(40) NOT NULL,
    "status"         varchar(8) NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'sent', 'paid', 'void')),
    "issued_on"      date NOT NULL,
    "due_on"         date NOT NULL,
    "period_from"    TIMESTAMP WITH TIME ZONE,
    "period_to"      TIMESTAMP WITH TIME ZONE,
    "currency"       char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    "seller"         jsonb NOT NULL DEFAULT '{}'::jsonb,
    "client"         jsonb NOT NULL DEFAULT '{}'::jsonb,
    "lines"          jsonb NOT NULL DEFAULT '[]'::jsonb,
    "subtotal_cents" bigint NOT NULL,
    "tax_percent"    numeric(5,2) NOT NULL DEFAULT 0,
    "tax_cents"      bigint NOT NULL,
    "total_cents"    bigint NOT NULL,
    "notes"          text NOT NULL DEFAULT '',
    "sent_at"        TIMESTAMP WITH TIME ZONE,
    "paid_at"        TIMESTAMP WITH TIME ZONE,
    "created_by"     uuid,
    "created_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- A number names one invoice, whatever its case.
CREATE UNIQUE INDEX IF NOT EXISTS invoices_number_idx ON invoices (lower(number));
CREATE INDEX IF NOT EXISTS invoices_project_idx ON invoices (project_id, issued_on DESC);
