// The location or category tree editor, embedded as a modal escape hatch for
// any location or category field rendered by a review screen
// (docs/specs/26-location-quick-create.md, docs/specs/27-category-quick-create.md).
//
// This is a second embedding of components locations.html and categories.html
// already use — js/tree.js itself, and the same create/rename/move calls
// against GET/POST/PATCH /api/storages/{storage_id}/locations or
// /categories (docs/specs/06-vision-shelf-ingestion.md,
// docs/specs/08-expiration-and-classification.md) — never a new tree UI and
// never a second same-storage validation path. The server is still the only
// thing that checks a node belongs to storageId; this module only ever calls
// it with the storageId its caller supplied, and never navigates the page.
//
// One module, parametrized by kind, instead of two nearly-identical ones
// (docs/specs/27-category-quick-create.md: "generalizes 26's modal instead
// of duplicating it"). The categories kind's shelf-life-rule detail beside
// each node is the exact renderer categories.html uses, from
// js/category-shelf-life.js — not a stripped-down copy of it.

import { TreeView } from "./tree.js";
import { get, post, patch, ApiError } from "./api.js";
import { el, text, clearChildren } from "./dom.js";
import { createShelfLifeDetail, resolveInheritance } from "./category-shelf-life.js";
import { t, apiErrorMessage } from "./i18n.js";

// The locations kind's DOM ids keep their pre-rename "location-modal-*"
// spelling on purpose: e2e/specs/ingestion.spec.js and
// e2e/specs/shopping-list.spec.js already hardcode
// "#location-modal-add-root-form" for the location-quick-create tests this
// module's predecessor shipped with, and this generalization changes the
// module's shape, not its locations behavior or its tests
// (docs/specs/27-category-quick-create.md). The categories kind, being new,
// gets its own "category-modal-*" ids below rather than inheriting these.
const KINDS = {
  locations: {
    endpoint: "locations",
    get title() { return t("locations.title"); },
    titleId: "location-modal-title",
    addRootFormId: "location-modal-add-root-form",
    get hint() { return t("locations.hint"); },
    get addRootLabel() { return t("locations.addRoot"); },
    get rootNameLabel() { return t("locations.addRootNameLabel"); },
    get rootNamePlaceholder() { return t("locations.addRootNamePlaceholder"); },
    get emptyMessage() { return t("treeModal.locations.empty"); },
  },
  categories: {
    endpoint: "categories",
    get title() { return t("categories.title"); },
    titleId: "category-modal-title",
    addRootFormId: "category-modal-add-root-form",
    get hint() { return t("categories.hint"); },
    get addRootLabel() { return t("categories.addRoot"); },
    get rootNameLabel() { return t("categories.addRootNameLabel"); },
    get rootNamePlaceholder() { return t("categories.addRootNamePlaceholder"); },
    get emptyMessage() { return t("treeModal.categories.empty"); },
  },
};

// True while a dialog opened by openTreeManager is on document.body, of
// either kind. location-options.js's openLocationField and
// category-options.js's openCategoryField each disable only their own
// trigger before calling openTreeManager, so nothing stops a location
// trigger's dialog and a category trigger's dialog from stacking at once
// (#144) — per-trigger disabling can't fix that, since neither caller knows
// about the other kind's trigger, so the guard lives here once instead.
let dialogOpen = false;

/**
 * openTreeManager renders js/tree.js inside a native <dialog>, scoped to
 * storageId and to one kind of tree. Resolves once the dialog is dismissed —
 * "Done", Esc, or a backdrop click, all equivalent — with the ids of any
 * nodes created during that session, in creation order.
 *
 * Same contract as docs/specs/26-location-quick-create.md's
 * openLocationManager(storageId) used to have, parametrized by which tree
 * js/tree.js renders inside the dialog.
 *
 * A second call of either kind, while a dialog from an earlier call is still
 * open, is refused rather than queued: it resolves immediately with no
 * created ids and never touches the DOM. Simpler than queuing, and nothing
 * in docs/specs/26 or docs/specs/27 asks for a second attempt to eventually
 * open once the first closes.
 *
 * `opened` is how a caller tells that refusal from a dialog that really ran
 * and happened to create nothing — both carry an empty createdIds, but only
 * the second is a reason to refresh anything. A caller that ignores it turns
 * a deliberate no-op into a network request, and a hiccup on that request
 * into an error message for a click the application chose not to act on
 * (#227).
 *
 * @param {string} storageId
 * @param {{kind: "locations"|"categories"}} options
 * @returns {Promise<{opened: boolean, createdIds: string[]}>}
 */
export function openTreeManager(storageId, { kind }) {
  if (dialogOpen) return Promise.resolve({ opened: false, createdIds: [] });
  dialogOpen = true;

  const config = KINDS[kind];
  const createdIds = [];
  // pendingMutations holds every runMutation() attempt still in flight (each
  // catches internally, so none ever rejects); requestClose waits until the
  // set is empty before finalizing, instead of resolving createdIds without
  // an id the create POST hasn't returned yet and racing the caller's own
  // refresh against that same in-flight write.
  //
  // A set rather than a single slot naming "the most recently started
  // mutation", because nothing orders POST responses by request order. With
  // a slot, an earlier-started mutation that settles *after* a later-started
  // one is lost: the later one finds the slot still naming itself, clears
  // it, and a requestClose waiting on it then sees an empty slot and
  // finalizes while the earlier one's create is still in flight — its id
  // never reaching createdIds and its late answer landing in a dialog
  // already removed (#221). Membership of a set is independent of settle
  // order, so the question "is anything still outstanding?" stops having a
  // wrong answer.
  //
  // An attempt stays in the set until its reload has redrawn the dialog too,
  // not merely until its own mutate() has answered (#280). Membership means
  // "this mutation is not finished with the dialog yet", and redrawing the
  // dialog is part of being finished with it: deleting on mutate()'s settle
  // left a window in which the set was already empty while the reload GET was
  // still outstanding, so a Done or Esc inside it finalized and that reload's
  // own view.render — or its showError — then landed on a dialog already
  // removed from the DOM.
  const pendingMutations = new Set();

  // unacknowledgedFailures holds one token per attempt whose error the user has
  // not yet been given the chance to read. A set of per-attempt tokens rather
  // than one shared boolean, for exactly the reason pendingMutations is a set:
  // a single flag names "the current mutation" in a function that can have
  // several in flight, and both ways that went wrong were real. A second
  // mutation starting reset the flag and cleared the banner, so a first
  // mutation's failure could disappear before anyone read it (#281); and
  // finalize consumed the flag, so of two dismissals asked for before the same
  // mutation failed, the first correctly kept the dialog open and the second
  // found the flag already cleared and closed it — in the same microtask batch,
  // before the error had ever been painted (#275).
  //
  // Nothing is ever removed from this set: a failure stops holding the dialog
  // open not by being consumed but by appearing in the snapshot requestClose
  // takes at the user's gesture (see requestClose), which is what tells a
  // failure the user has had in front of them from one that appeared while
  // their dismissal was already waiting.
  const unacknowledgedFailures = new Set();

  const errorBox = el("div", { class: "alert", role: "alert", hidden: true });
  const statusBox = el("p", { class: "empty-state", role: "status", hidden: true });
  const hint = el("p", { class: "empty-state" }, [text(config.hint)]);
  const addRootButton = el("button", { type: "button", class: "btn" }, [text(config.addRootLabel)]);
  const treeContainer = el("div");
  const doneButton = el("button", { type: "button", class: "btn btn--primary" }, [text(t("treeModal.done"))]);

  const dialog = el("dialog", { class: "card stack", "aria-labelledby": config.titleId }, [
    el("h2", { id: config.titleId }, [text(config.title)]),
    errorBox,
    statusBox,
    el("div", { class: "row row--between" }, [hint, addRootButton]),
    treeContainer,
    el("div", { class: "row" }, [doneButton]),
  ]);

  function basePath() {
    return `/api/storages/${storageId}/${config.endpoint}`;
  }

  // inherited is only ever populated for the categories kind (see reload
  // below); the locations kind's renderDetail stays unset here, as it did
  // on this module's predecessor (js/location-modal.js) before this spec.
  // locations.html's own page-level TreeView does pass one — the audit
  // staleness detail (docs/specs/13-stocktake-and-audit.md) — but that was
  // never part of the modal's own contract (docs/specs/26-location-quick-create.md)
  // and adding it here is out of this spec's scope.
  let inherited = new Map();

  const view = new TreeView(treeContainer, {
    onAddChild: (parentId, name) => runMutation(() => createNode(parentId, name)),
    onRename: (id, name) => runMutation(() => patch(`${basePath()}/${id}`, { name })),
    onMove: (id, newParentId) => runMutation(() => patch(`${basePath()}/${id}`, { parent_id: newParentId })),
    renderDetail:
      kind === "categories"
        ? createShelfLifeDetail({ basePath, runMutation, showStatus, getInherited: () => inherited })
        : undefined,
  });

  function createNode(parentId, name) {
    return post(basePath(), { name, parent_id: parentId }).then((created) => {
      createdIds.push(created.id);
    });
  }

  // The server validates every mutation — same-storage membership, the cycle
  // rule on a move — so the dialog redraws from its answer rather than
  // predicting the new shape, exactly as locations.js and categories.js do.
  async function runMutation(mutate) {
    // The status line belongs to whichever mutation is being started, so it is
    // always replaced. An error banner is not: it may be an earlier mutation's
    // failure, and clearing it here is precisely how a second mutation used to
    // erase a first one's failure — banner and flag together — leaving Done
    // free to close the dialog with the user never having been told that the
    // first mutation did not happen (#281).
    //
    // Be clear about what that costs, because it is not a temporary state:
    // nothing ever removes a token from unacknowledgedFailures — there is no
    // .delete anywhere in this file — so once any mutation has failed, this
    // guard stops clearing the banner for the rest of the dialog's life. A
    // failed create's message therefore outlives every later *successful*
    // mutation and goes only when the dialog closes, which can leave it
    // standing, stale but true, beside a success.
    //
    // That is deliberate and it is forced: keeping a failure visible until the
    // user has been given the chance to read it and clearing it as soon as the
    // next mutation succeeds are the same decision with opposite answers, and
    // #281 is the bug report for choosing the second. It is also the second of
    // the two shapes #281 itself offers — "clear only messages belonging to the
    // mutation being started, or nothing at all while an unacknowledged failure
    // exists". A banner that lingers can be misread as a later mutation's
    // failure; a banner that vanishes hides that an earlier one never happened.
    // The first is the cheaper mistake. Do not add a read/acknowledge
    // transition here to tidy the staleness away — that is the regression
    // #281 was filed over.
    clearStatus();
    if (unacknowledgedFailures.size === 0) clearError();

    // One token per attempt, so both sets track attempts by identity rather
    // than by "the latest one". The tracked promise deliberately covers
    // mutate() *and* the reload that redraws the dialog from its answer, so
    // membership of pendingMutations lasts as long as this attempt still has
    // something to do to the dialog (#280).
    const token = {};
    const attempt = (async () => {
      try {
        await mutate();
      } catch (err) {
        // Latched the moment the banner is painted, which is what makes the
        // snapshot requestClose takes meaningful: a dismissal asked for after
        // this point has had the error in front of it, one already waiting when
        // it appeared has not.
        showError(err);
        unacknowledgedFailures.add(token);
      }
      // reload() reports instead of throwing, because it is also the initial
      // render's path and has nobody to throw to there. Its failure is latched
      // the same way a mutation's is: it paints the same banner in the same
      // dialog, so a finalize that closed over it unseen would be the same bug
      // in a different place.
      if (!(await reload())) unacknowledgedFailures.add(token);
    })();

    pendingMutations.add(attempt);
    try {
      await attempt;
    } finally {
      // Leaves the set the moment the attempt settles — its reload included —
      // whoever else is still in it and whenever they started, which is the
      // whole point of tracking membership rather than "the latest one".
      pendingMutations.delete(attempt);
    }
  }

  /**
   * reload re-reads the tree and redraws the dialog from the server's answer.
   *
   * @returns {Promise<boolean>} true when the redraw happened, false when the
   *   GET failed and showError put a banner up in its place.
   */
  async function reload() {
    try {
      const body = await get(basePath());
      const empty = body.items.length === 0;
      hint.hidden = empty;
      if (empty) {
        clearChildren(treeContainer);
        treeContainer.append(el("p", { class: "empty-state" }, [text(config.emptyMessage)]));
        return true;
      }
      if (kind === "categories") inherited = resolveInheritance(body.items);
      view.render(body.items);
      return true;
    } catch (err) {
      showError(err);
      return false;
    }
  }

  function showError(err) {
    errorBox.textContent = err instanceof ApiError ? apiErrorMessage(err) : t("treeModal.networkError");
    errorBox.hidden = false;
  }

  function showStatus(message) {
    statusBox.textContent = message;
    statusBox.hidden = false;
  }

  function clearError() {
    errorBox.textContent = "";
    errorBox.hidden = true;
  }

  function clearStatus() {
    statusBox.textContent = "";
    statusBox.hidden = true;
  }

  // showAddRootForm is the one case the tree component cannot cover: its
  // inline add-child editor opens underneath an existing node, and a first
  // top-level node has none to open under. Same inline pattern locations.js
  // and categories.js use, for the same reason (no prompt(), styleable,
  // reachable by assistive tech).
  addRootButton.addEventListener("click", () => {
    if (dialog.querySelector(`#${config.addRootFormId}`)) return;

    const input = el("input", {
      type: "text",
      "aria-label": config.rootNameLabel,
      placeholder: config.rootNamePlaceholder,
      required: true,
    });

    const form = el(
      "form",
      {
        id: config.addRootFormId,
        class: "row",
        onsubmit: (event) => {
          event.preventDefault();
          const name = input.value.trim();
          if (!name) return;
          form.remove();
          runMutation(() => createNode(null, name));
        },
      },
      [
        input,
        el("button", { type: "submit", class: "btn" }, [text(t("tree.add"))]),
        el("button", { type: "button", class: "btn btn--ghost", onclick: () => form.remove() }, [text(t("common.cancel"))]),
      ],
    );

    treeContainer.before(form);
    input.focus();
  });

  // requestClose is every dismissal's shared path — Done, a genuine backdrop
  // click, and Esc all route through it, so all three wait the same way for
  // a mutation that's still in flight instead of resolving createdIds without
  // it. If that mutation ended in an error, the dialog stays open on this
  // request so the user actually sees showError's message rather than it
  // landing in a dialog already removed; dismissing again — the error now in
  // front of them — closes it.
  //
  // INVARIANT: finalize only with pendingMutations empty, and re-check it
  // after every wait instead of finalizing once the batch awaited here
  // settles. Nothing stops a further mutation from starting while this
  // function is already waiting (the add-root form reopens the moment it's
  // submitted, before its own POST returns); such a mutation joins the set
  // and has to be waited for too. Recursing until the set is genuinely empty
  // is what makes both who-started-last and who-settles-last irrelevant —
  // finalizing on the first settled batch would resolve createdIds without
  // whatever a newer mutation is still in the middle of creating.
  //
  // The gesture is also where the set of failures the user has already been
  // shown is captured, once, and then carried unchanged through every wait
  // below. That snapshot is what separates "a failure that was on screen when
  // the user asked to close" from "a failure that appeared while this dismissal
  // was waiting", and it does so without consuming anything — which is exactly
  // what two dismissals racing one failing mutation need, since both of them
  // reach finalize in the same microtask batch and a consume-once flag can only
  // stop one of them (#275).
  function requestClose() {
    waitThenFinalize(new Set(unacknowledgedFailures));
  }

  function waitThenFinalize(seenFailures) {
    if (pendingMutations.size === 0) {
      finalize(seenFailures);
      return;
    }
    Promise.all([...pendingMutations]).then(() => waitThenFinalize(seenFailures));
  }

  // A failed mutation keeps the dialog open on every dismissal that was asked
  // for before its error was painted, so the user actually sees showError's
  // message instead of it landing in a dialog already removed. The next
  // dismissal — made with the banner in front of the user — carries that
  // failure in its own snapshot and closes normally, so a failure can never
  // trap the dialog open either.
  //
  // Only the dialog staying open is guaranteed, not that every message is
  // individually legible: two failures share the one error box, so the later
  // one's text replaces the earlier one's. Both tokens are latched, so the
  // dialog still refuses to close over either of them.
  function finalize(seenFailures) {
    for (const failure of unacknowledgedFailures) {
      if (!seenFailures.has(failure)) return;
    }
    dialog.close();
  }

  return new Promise((resolve) => {
    doneButton.addEventListener("click", requestClose);

    // A click that lands on the dialog element itself, rather than on any of
    // its children, is USUALLY a click on the backdrop — but .card puts its
    // padding on the <dialog> element (css/components.css), so a click on
    // that padding also has event.target === dialog without being outside
    // the dialog's own box. Comparing the click's coordinates against the
    // dialog's rendered box is what actually tells backdrop and padding
    // apart.
    dialog.addEventListener("click", (event) => {
      if (event.target !== dialog) return;
      const box = dialog.getBoundingClientRect();
      const insideBox =
        event.clientX >= box.left &&
        event.clientX <= box.right &&
        event.clientY >= box.top &&
        event.clientY <= box.bottom;
      if (!insideBox) requestClose();
    });

    // Esc fires the dialog's own "cancel" then "close" natively; preventing
    // the default on "cancel" routes it through requestClose too, so Esc
    // waits for an in-flight mutation exactly like Done and the backdrop do.
    dialog.addEventListener("cancel", (event) => {
      event.preventDefault();
      requestClose();
    });

    dialog.addEventListener("close", () => {
      dialog.remove();
      dialogOpen = false;
      resolve({ opened: true, createdIds: [...createdIds] });
    });

    document.body.append(dialog);
    dialog.showModal();
    // The initial render, deliberately not tracked in pendingMutations and its
    // answer deliberately not latched. Unlike a mutation's reload, a dismissal
    // is allowed to win the race against this one: Done, the backdrop and Esc
    // are all wired above, *before* showModal(), so a dismissal genuinely can
    // arrive while this GET is still in flight — requestClose then finds the set
    // empty, finalize closes the dialog, and this reload's view.render or
    // showError afterwards runs against a treeContainer and errorBox already
    // detached from the document.
    //
    // That is the same shape as #280 and it is still the behaviour wanted here,
    // which is why #280's own body excludes this call site: "reload() is also
    // called from other places that are not mutations (the initial render), so
    // the fix belongs in runMutation's tracking rather than inside reload()
    // itself." Nothing is at stake in this window — no mutation has run, so
    // createdIds is empty by construction and there is no answer the user is
    // still owed — while tracking it would make Esc do nothing at all until a
    // slow opening GET came back, which is a real cost for no gain. A banner
    // painted into a dialog the user has already dismissed is likewise not
    // something finalize should hold anything open for.
    reload();
  });
}
