# Arcus Investments — User Manual

For the people who use the system to run the business. No technical background assumed.

If you only read one thing, read [Money in and money out](#the-shape-of-it) — it explains why the
system asks you to do things in a particular order, which is the part that most often feels like
extra work and is the part that keeps the numbers honest.

---

## Contents

1. [The shape of it](#the-shape-of-it)
2. [Signing in](#signing-in)
3. [Selling: lead to payment](#selling-lead-to-payment)
4. [Buying: order to landed cost](#buying-order-to-landed-cost)
5. [Knowing what you made: margin per deal](#knowing-what-you-made-margin-per-deal)
6. [The counter: walk-in sales and the till](#the-counter-walk-in-sales-and-the-till)
7. [Stock](#stock)
8. [Approvals: when the system says no](#approvals-when-the-system-says-no)
9. [Printing documents](#printing-documents)
10. [People and permissions](#people-and-permissions)
11. [Things that are meant to work this way](#things-that-are-meant-to-work-this-way)
12. [When something looks wrong](#when-something-looks-wrong)

---

## The shape of it

The system has a sell side and a buy side, and they meet at margin.

**Selling:** a lead becomes a deal, the deal produces a quotation, the client accepts, you invoice
them, they pay, you issue a receipt.

**Buying:** you decide what to buy, raise a purchase order, get it approved, send it to the supplier,
the goods arrive, the supplier's bill arrives, and the freight and duty get added into what the goods
actually cost.

**Margin** is the sell side less the buy side, for one deal.

The single most important idea: **ordering, receiving and being invoiced are three separate things**,
and the system keeps them separate on purpose. A container can arrive in two shipments and be invoiced
once. A supplier can demand payment before anything ships. If the system made you do all three at
once, you would eventually be stuck unable to receive goods that are sitting in your yard because the
invoice has not arrived — so it does not.

---

## Signing in

Go to the site and sign in with your work email. If you have forgotten your password, use **Forgot
password** — it emails you a link. Links expire, so request a fresh one rather than reusing an old
email.

**If a section you expect is missing from the left-hand menu, you do not have permission to it.** The
menu only shows what your role may reach. That is not a fault — ask whoever administers users to grant
it. See [People and permissions](#people-and-permissions).

If you have just been granted a new permission and still cannot see the section, **sign out and sign
back in**. Your permissions are loaded when you sign in, so a change made while you are logged in does
not reach you until you start a new session.

---

## Selling: lead to payment

### A lead arrives

Enquiries from the website land in **Intake**. Read it, then either discard it or **Convert** it into
a deal. Converting carries the contact details across so you do not retype them.

### Working a deal

**Pipeline** is the deal board, one column per stage. Drag a deal between columns as it progresses, or
open it and change the stage.

Open a deal and you can set:

- **Deal value** — what you expect it to be worth.
- **Stage** and **probability** — probability defaults from the stage; override it when you know
  better.
- **Grade** and **segment** — how important this one is.
- **Line items** — the actual things you are selling, with quantity and unit price. **Fill these in.**
  Quotations and invoices are built from them, and margin is compared against them. A deal with only a
  total is a deal the system cannot document or cost.
- **Contacts** — everyone involved on the client side. Mark one as primary; that is who documents are
  addressed to.
- **Engagement log** — every call, meeting and email. This is the part that saves you when a deal comes
  back to life eight months later.

### Quotation, invoice, receipt

Inside a deal, under **Documents & Payments**, are buttons for **Quotation** and **Invoice**. They
generate a printable document from the line items, on the Arcus letterhead.

The **Apply VAT** toggle sets whether the document carries VAT at 16%. It is remembered on the deal, so
the invoice the client receives and the balance the system chases cannot disagree.

**Invoice the deal** stamps the date the client was actually billed. Do this when you really send the
invoice, not before — the debtor book ages from this date, and a deal with no invoice date is not yet a
receivable.

### Getting paid

Record payments against the deal as they arrive: amount, method, reference, date. The balance is
worked out from the invoice total less what has been recorded; it is never stored, so it cannot drift.

Each recorded payment gets its own **Receipt** button.

> **The system records payments. It does not take them.** There is no card processing and no funds
> transfer anywhere in it. Recording a payment is bookkeeping — it does not move money.

### Chasing

**Receivables** is the aged debtor book: who owes what, and how overdue, in ageing buckets. It is
computed live from invoices and payments every time you open it. There is a CSV export for your
accountant.

---

## Buying: order to landed cost

This is the part that tells you what things really cost. For a business that imports and resells, a
supplier's price is not the cost — freight, duty, clearing and handling are real and often large.
Selling at a "40% margin" on the supplier's price can be a loss.

### Step 1 — Compare suppliers (optional)

**Purchasing → Sourcing** holds several supplier quotes for the same requirement side by side, and
records which you chose **and why**. For government and parastatal work that written comparison is an
audit document, not just a cost control.

Whether a minimum number of quotes is *required* is a setting, not a rule baked into the system. Some
parts have one distributor in the country and insisting on three quotes would just stop work.

### Step 2 — Raise the purchase order

**Purchasing → New purchase order.** Fill in:

- **Supplier**, and their TPIN if they are Zambian. A foreign supplier has no TPIN; leave it empty.
- **Currency** and **exchange rate**. Enter the supplier's price in **their** currency — USD, ZAR, CNY
  — and the rate you are using. Do not convert it yourself. The system keeps the foreign figure, the
  rate and the kwacha result, which is the only way the kwacha number can be explained or corrected
  later.
- **Incoterms** — EXW, FOB, CIF, DDP. Worth getting right: it tells you which costs are still coming.
  On DDP the price already includes freight and duty; on EXW almost nothing is included.
- **Expected delivery**, and the deal this is for if it is for a specific job.
- **Lines** — description, quantity, unit price. Link a line to a catalogue product where there is
  one; leave it unlinked for a service or a one-off, which is normal.

It saves as a **draft**. A draft commits you to nothing and can be edited freely.

### Step 3 — Get it approved and issue it

**Submit for approval**, then once approved, **Issue**. Issuing allocates the PO number and is the
point at which the order becomes a promise the supplier can hold you to — which is why the approval
sits here rather than on saving a draft.

If the value is over a configured threshold you will be told it needs approval and who from. That is
not an error. See [Approvals](#approvals-when-the-system-says-no).

Only an **approved** order can be issued, and only an **issued** order can receive goods.

### Step 4 — Receive the goods

When the goods physically arrive, open the order and **Receive goods**. Enter the quantity that
actually turned up **per line**, the delivery note number, and the date.

**Partial delivery is normal and fully supported.** Received 6 of 10? Enter 6. The order shows as
partly received and the remaining 4 stay outstanding. When the rest arrives, record a second receipt.
Each receipt is its own event with its own date, and can carry its own freight.

Receiving is what puts the goods into stock. Nothing else does.

> Record what arrived, not what was ordered. If the two differ, that difference is the thing you most
> need the system to know.

### Step 5 — Add the landed costs

On the receipt, add each charge that brought the consignment in: **freight, insurance, customs duty,
clearing agent, port and handling**. For each, enter the amount, its currency and its rate, and the
invoice reference.

Then choose the **apportionment basis** and apply it:

| Basis | Use it when |
| --- | --- |
| **Value** | The default. Simple, conventional, easy to explain. |
| **Weight** | Freight on a mixed consignment. More honest when you are shipping something cheap and heavy alongside something dear and light. |
| **Quantity** | Everything in the consignment is much the same. |

The system spreads the charges across the received lines and writes the result into each item's unit
cost. The basis is saved with the receipt, so anyone can re-derive the arithmetic later.

Two things worth knowing:

- **The shares always add back to exactly what you were charged.** Splitting a lump sum across lines
  never divides evenly; the leftover ngwee is placed deliberately rather than lost, so the total never
  drifts.
- **A late invoice is fine.** Clearing agents bill weeks later. Add the charge when it arrives and
  re-apply — the individual charges are kept, not just the total, so it recalculates instead of needing
  a rebuild.

### Step 6 — Book the supplier's invoice

When the bill arrives, record it in **Payables** as an expense, linked to the purchase order. Then
record settlements against it as you pay.

**On VAT, read this carefully — it is money.**

- For a **Zambian supplier**, enter the **Smart Invoice reference**. Without it, ZRA will not accept
  the input VAT claim, so the VAT is a cost rather than something you get back. The system reports it
  that way.
- For an **import**, enter the **customs assessment reference** instead. Import VAT is paid at the
  border and evidenced by the customs entry. A foreign supplier cannot issue a Smart Invoice and never
  will.

Do not put a customs number in the Smart Invoice field. They are different documents proving different
things, and the system reports them differently on purpose.

---

## Knowing what you made: margin per deal

Once the buy side is recorded, open the deal in **Pipeline**. The margin block shows:

- the deal value,
- less cost of goods at **landed** cost — including the freight and duty,
- less expenses booked directly against the deal,
- leaving margin, in kwacha and as a percentage.

For this to be right, two habits matter:

1. **Attribute costs to the deal.** When recording an expense or raising a purchase order for a
   specific job, set the deal. An unattributed cost is real money that will not appear in any deal's
   margin.
2. **Record the landed costs before you judge the margin.** A margin calculated before the freight is
   entered is flattering and wrong.

---

## The counter: walk-in sales and the till

For over-the-counter selling.

1. **Open a till session** at the start of a shift with your opening float.
2. Ring up sales: pick products, quantities, take payment. Each sale prints a receipt and takes the
   items out of stock.
3. **Close the session** at the end, entering what is actually in the drawer. The system compares that
   with what should be there and records the difference.

Count the drawer and enter the true figure. A recorded discrepancy is a fact someone can look into; a
fudged one hides the thing you needed to find.

---

## Stock

**Catalogue** holds the products. **Stock ledger** shows every movement.

Quantity on hand is always the **sum of the movements** — there is no stored stock number to correct,
and no way for the figure to disagree with its own history. Every movement records what changed, why,
when and who did it.

| Kind | Meaning |
| --- | --- |
| Opening | What was on hand when the ledger started |
| Receipt | Goods in from a supplier — carries the landed unit cost |
| Sale | Goods out to a customer |
| Return | Goods back from a customer |
| Adjustment | A stock count correcting the books |
| Write-off | Damaged, expired or missing |

**Use the right kind.** A shortfall recorded as a write-off is a loss worth investigating; the same
shortfall recorded as a sale is a good week. The number is identical and the meaning is opposite.
Adjustments and write-offs require a reason, because the number alone explains nothing.

---

## Approvals: when the system says no

Some actions need a second person: issuing a large purchase order, closing a big deal, recording or
settling a large expense, signing a contract. The thresholds are configurable — "over K X needs N
approvals from role R".

When you attempt a gated action you get a message saying it needs approval, and a request goes to the
people who can give it. **This is not an error and not a rejection.** Once approved, go back and do
the action again — approving does not perform it for you, deliberately. For something like signing,
the signature has to come from the real signer at the real moment, not from an approver's click.

If it is **rejected**, you will see the reason. Revise and resubmit rather than retrying the same
thing; a resubmission is linked to what it replaces so the history reads as a conversation.

An approval is for **that person, that action, that record, up to that amount**. An approval for
K50,000 will not admit a K500,000 version of the same action.

---

## Printing documents

Five printable documents: **quotation, invoice, receipt, purchase order** and the **till receipt**.

Open the document and use **Print / Save as PDF**. Choose "Save as PDF" as the destination to keep a
copy. What you see in that dialog is what the client or supplier gets — the letterhead, the line
table, the totals.

**Pressing Ctrl+P without a document open gives you a blank page.** That is deliberate: it is what
stops the menus and the workspace ending up on a client's invoice. Open the document first.

Press **Escape** or **Close** to leave a document. Anything you were part-way through filling in behind
it is still there.

---

## People and permissions

**Users** lists staff accounts. You can invite someone, change their role, and deactivate them.

Permissions are per **resource** (deals, contracts, payments, purchase orders, expenses, stock, users,
and so on) and per **action** (read, create, update, delete). Built-in roles cover the common cases;
custom roles let you build something specific.

Two things to know:

- **Reads can be scoped.** A role can be limited to only its own records rather than everything.
- **Custom roles do not pick up new features automatically.** When a new area is added to the system,
  built-in roles get it, but a custom role has to be granted it by hand. If someone on a custom role
  cannot see a section everyone else can, that is why.

Money out is deliberately separate from money in: someone trusted to record what a client paid is not
automatically trusted to record and pay supplier invoices.

---

## Things that are meant to work this way

Behaviour that looks like a bug and is not:

- **The system does not move money.** Payments and settlements are recorded. There is no gateway.
- **Printing with no document open gives a blank page.** See [Printing](#printing-documents).
- **A blocked action is not a failure.** It is an approval request. See
  [Approvals](#approvals-when-the-system-says-no).
- **Balances and ageing are never stored.** They are recomputed whenever you look, from invoices and
  payments, so they cannot silently drift.
- **Stock has no stored total.** It is the sum of its movements.
- **A PO number appears only on issue.** Drafts have none, so abandoned drafts do not leave gaps in a
  sequence an auditor expects to be unbroken.
- **Wide tables scroll sideways.** On a phone, swipe the table itself. A shadow on its right edge means
  there is more to see.
- **Missing menu sections are permissions**, not faults — and a newly granted permission needs a fresh
  sign-in.

---

## When something looks wrong

Work down this list before reporting it:

1. **A section is missing.** Permissions. Sign out and back in first, then ask for the grant.
2. **A figure looks wrong on a deal's margin.** Check the landed costs are entered on the receipt and
   apportioned, and that the expenses are attributed to that deal. Margin can only see what has been
   recorded and attributed.
3. **Stock is wrong.** Open the stock ledger for that product and read the movements. The total is
   their sum, so the wrong entry is visible — usually a receipt recorded for the ordered quantity
   rather than the delivered one.
4. **VAT looks unrecoverable when it should not be.** Check the right evidence field: Smart Invoice
   reference for a Zambian supplier, customs assessment reference for an import.
5. **An outstanding order will not close.** Check the received quantity per line. Partial receipts are
   supposed to leave a remainder outstanding.
6. **You cannot do something you used to be able to do.** Check whether it is now gated on value — the
   thresholds can be changed.

When reporting a problem, say what you were doing, what you expected, and what happened. If a figure
is wrong, give the document or reference number — it is far quicker to trace a specific record than a
description.
