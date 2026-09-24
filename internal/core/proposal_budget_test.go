// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"strings"
	"testing"
	"time"
)

// TestProposalBudgetCapsAdminMailPerHolder — #15, audit M5, owner
// decision 2026-09-24 (Option A): every proposal pages the admin and is
// stored for good, so one leaked portal or action link could send the
// operator a mail per submit and grow state.Proposals without bound. A
// person holds at most MaxOpenProposalsPerPerson undecided proposals,
// across every entry point; deciding one is the operator acting, and
// frees the budget.
func TestProposalBudgetCapsAdminMailPerHolder(t *testing.T) {
	svc, _ := newTestService(t, &fakeNotifier{})
	e := mustEvent(t, svc, 40*time.Hour)
	p := mustPerson(t, svc, "ana", TrustPropose)
	other := mustPerson(t, svc, "bo", TrustPropose)
	svc.Assign(e.ID, p.ID, "")
	svc.Assign(e.ID, other.ID, "")
	start := svc.now().Add(60 * time.Hour)
	submit := func(token string) error {
		_, err := svc.PortalSubmit(token, "move", e.ID, "", "clash", start, start.Add(time.Hour))
		return err
	}

	for i := range MaxOpenProposalsPerPerson {
		if err := submit(p.PortalToken); err != nil {
			t.Fatalf("submit %d inside the budget was refused: %v", i+1, err)
		}
	}
	for range 5 {
		if err := submit(p.PortalToken); err == nil {
			t.Fatal("a portal submit past the budget was filed")
		}
	}
	_, cancelURL, _ := svc.GenerateLinks(e.ID, p.ID)
	link := strings.TrimPrefix(cancelURL, "http://test.local/a/")
	if _, err := svc.ProposeMoveViaLink(link, start, start.Add(time.Hour), "clash"); err == nil {
		t.Fatal("the action link is a second way past the budget")
	}
	adminMails := 0
	for _, o := range svc.OutboxItems(false) {
		if o.Subject == "Proposal from ana" {
			adminMails++
		}
	}
	if adminMails != MaxOpenProposalsPerPerson {
		t.Fatalf("want %d admin mails queued, got %d", MaxOpenProposalsPerPerson, adminMails)
	}
	if got := len(svc.Proposals()); got != MaxOpenProposalsPerPerson {
		t.Fatalf("want %d stored proposals, got %d", MaxOpenProposalsPerPerson, got)
	}

	// Someone else's budget is their own.
	if err := submit(other.PortalToken); err != nil {
		t.Fatalf("one holder's budget blocked another: %v", err)
	}

	// The operator decides one: one slot comes back, not more.
	if _, err := svc.DecideProposal(svc.Proposals()[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if err := submit(p.PortalToken); err != nil {
		t.Fatalf("a decided proposal did not free the budget: %v", err)
	}
	if err := submit(p.PortalToken); err == nil {
		t.Fatal("one decision freed more than one slot")
	}
}
