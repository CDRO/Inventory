#!/usr/bin/perl
# Checks and edits the Build Runbook page for /seed-issues, step 4.
#
#   perl runbook.pl check   page.html
#   perl runbook.pl extract page.html plan.json state.json
#   perl runbook.pl inject  page.html plan.json state.json out.html
#
# `check` exits 1 and names every problem when the page is not the page's own
# clean markup. `inject` refuses to write unless the source page is clean, the
# edited blocks keep every rule step 4 states, and the result checks clean too.
#
# Core Perl modules only: Git Bash ships perl, so this needs no host toolchain.
use strict;
use warnings;
use JSON::PP;

$| = 1;
my %STATUSES = map { $_ => 1 } qw(todo working done);

my ($cmd, @args) = @ARGV;
if    (($cmd // '') eq 'check'   && @args == 1) { exit(report(check_page(slurp($args[0]))) ? 0 : 1) }
elsif (($cmd // '') eq 'extract' && @args == 3) { extract(@args) }
elsif (($cmd // '') eq 'inject'  && @args == 4) { inject(@args) }
else  { die "usage: runbook.pl check page.html | extract page.html plan.json state.json | inject page.html plan.json state.json out.html\n" }

sub slurp {
  my ($file) = @_;
  open my $fh, '<:raw', $file or die "cannot read $file: $!\n";
  local $/;
  my $s = <$fh>;
  close $fh;
  return $s;
}

sub spit {
  my ($file, $s) = @_;
  open my $fh, '>:raw', $file or die "cannot write $file: $!\n";
  print $fh $s;
  close $fh or die "cannot write $file: $!\n";
}

# scripts walks the page's script elements in document order. A script's
# content ends at the first "</script" after its opening tag, which is where a
# browser ends it too, so a "<script" string inside one (the page's own
# buildDoc has one) is content, never a second element.
sub scripts {
  my ($html) = @_;
  my @found;
  while ($html =~ /<script\b([^>]*)>(.*?)<\/script\s*>/gis) {
    my %s = (raw => $1, body => $2, body_start => $-[2], body_end => $+[2]);
    $s{id} = $1 if $s{raw} =~ /\bid\s*=\s*"([^"]*)"/i;
    push @found, \%s;
  }
  return @found;
}

# check_page returns the problems with a page, and a summary of it when there
# are none.
sub check_page {
  my ($html) = @_;
  my @problems;

  # The viewer's runtime, baked in by a save that serialized the live DOM.
  push @problems, 'the viewer runtime is baked into the page (frame-runtime / FRAME_PREAMBLE)'
    if $html =~ /frame-runtime|FRAME_PREAMBLE/;

  my @scripts = scripts($html);

  # Every script the page writes carries data-rb. Anything else was injected:
  # the viewer, a browser extension, or a hand edit.
  for my $s (@scripts) {
    push @problems, 'a script the page did not write: <script' . substr($s->{raw}, 0, 80) . '>'
      unless $s->{raw} =~ /\bdata-rb\b/;
  }

  (my $outside = $html) =~ s/<script\b[^>]*>.*?<\/script\s*>//gis;
  push @problems, 'a <script> with no closing tag' if $outside =~ /<script\b/i;
  push @problems, 'a </script> with no opening tag: a script ended early' if $outside =~ /<\/script/i;

  # A save that doubled the page doubles its marked nodes, so no marked node
  # may appear twice. Scripts count by id, the untyped app script as "app".
  my %seen;
  $seen{ 'script#' . ($_->{id} // 'app') }++ for @scripts;
  while ($outside =~ /<([a-z][a-z0-9]*)\b([^>]*\bdata-rb\b[^>]*)>/gi) {
    (my $key = lc($1) . ' ' . $2) =~ s/\s*\bdata-rb(="")?//;
    $key =~ s/\s+/ /g;
    $key =~ s/\s+$//;
    $seen{$key}++;
  }
  for my $key (sort keys %seen) {
    push @problems, "marked node appears $seen{$key} times: $key" if $seen{$key} > 1;
  }
  for my $key ('script#plan', 'script#state', 'script#app') {
    push @problems, "missing $key" unless $seen{$key};
  }
  return (\@problems) if @problems;

  my ($plan_script)  = grep { ($_->{id} // '') eq 'plan' } @scripts;
  my ($state_script) = grep { ($_->{id} // '') eq 'state' } @scripts;
  my ($plan, $plan_problems)   = parse_block('plan', $plan_script->{body});
  my ($state, $state_problems) = parse_block('state', $state_script->{body});
  push @problems, @$plan_problems, @$state_problems;
  push @problems, check_data($plan, $state) if $plan && $state;
  return (\@problems) if @problems;

  my $items = () = map { @{ $_->{items} } } @{ $plan->{phases} };
  my $summary = sprintf 'clean: %d phases, %d items, %d statuses, updated %s',
    scalar @{ $plan->{phases} }, $items, scalar keys %{ $state->{status} }, $state->{updated} // '(never)';
  return (\@problems, $summary, $plan, $state, $plan_script, $state_script);
}

# parse_block decodes one data block as the page stores it: UTF-8 JSON with no
# literal "<", since that is what keeps text from closing the script tag.
sub parse_block {
  my ($name, $body) = @_;
  my @problems;
  push @problems, "the $name block contains a literal \"<\"; every one must be escaped as \\u003c" if $body =~ /</;
  my $data = eval { JSON::PP->new->utf8->decode($body) };
  push @problems, "the $name block is not valid JSON: $@" unless defined $data;
  return ($data, \@problems);
}

# check_data holds the rules step 4 states for the two blocks.
sub check_data {
  my ($plan, $state) = @_;
  my @problems;

  my %ids;
  if (ref $plan ne 'HASH' || ref $plan->{phases} ne 'ARRAY') {
    return ('plan must be {"phases":[...]}');
  }
  for my $phase (@{ $plan->{phases} }) {
    if (ref $phase ne 'HASH' || ref $phase->{items} ne 'ARRAY') {
      push @problems, 'every phase needs an items array';
      next;
    }
    for my $field (qw(key name)) {
      push @problems, "a phase is missing \"$field\"" unless defined $phase->{$field} && length $phase->{$field};
    }
    for my $item (@{ $phase->{items} }) {
      my $id = ref $item eq 'HASH' ? $item->{id} : undef;
      if (!defined $id || $id !~ /^[a-z]+[0-9]+$/) {
        push @problems, 'an item has no id of the form m<n>, f<n> or s<n>';
        next;
      }
      push @problems, "item id $id is used twice" if $ids{$id}++;
      for my $field (qw(n spec title desc)) {
        push @problems, "item $id is missing \"$field\"" unless defined $item->{$field} && length $item->{$field};
      }
      push @problems, "item $id has no steps" unless ref $item->{steps} eq 'ARRAY' && @{ $item->{steps} };
    }
  }

  if (ref $state ne 'HASH' || ref $state->{status} ne 'HASH') {
    push @problems, 'state must be {"status":{...},"updated":"..."}';
    return @problems;
  }
  for my $id (sort keys %{ $state->{status} }) {
    push @problems, "state names $id, which is no item in the plan" unless $ids{$id};
    push @problems, "state for $id is \"$state->{status}{$id}\", not todo, working or done"
      unless $STATUSES{ $state->{status}{$id} // '' };
  }
  return @problems;
}

sub report {
  my ($problems, $summary) = @_;
  if (@$problems) {
    print STDERR "NOT CLEAN, do not republish this copy:\n";
    print STDERR "  - $_\n" for @$problems;
    return 0;
  }
  print "$summary\n";
  return 1;
}

sub extract {
  my ($page, $plan_file, $state_file) = @_;
  my ($problems, $summary, $plan, $state) = check_page(slurp($page));
  report($problems, $summary) or exit 1;
  my $json = JSON::PP->new->utf8->pretty->canonical;
  spit($plan_file, $json->encode($plan));
  spit($state_file, $json->encode($state));
}

sub inject {
  my ($page, $plan_file, $state_file, $out) = @_;
  my $html = slurp($page);
  my ($problems, $summary, $old_plan, $old_state, $plan_script, $state_script) = check_page($html);
  report($problems, $summary) or exit 1;

  my @refusals;
  my $plan  = eval { JSON::PP->new->utf8->decode(slurp($plan_file)) }  or push @refusals, "$plan_file is not valid JSON: $@";
  my $state = eval { JSON::PP->new->utf8->decode(slurp($state_file)) } or push @refusals, "$state_file is not valid JSON: $@";
  if ($plan && $state) {
    push @refusals, check_data($plan, $state);

    # Ids are permanent: the state keys on them, and a person's status on a
    # removed id is lost without a trace.
    my %new_ids = map { $_->{id} => 1 } grep { ref eq 'HASH' && defined $_->{id} }
      map { ref $_ eq 'HASH' && ref $_->{items} eq 'ARRAY' ? @{ $_->{items} } : () } @{ $plan->{phases} // [] };
    for my $phase (@{ $old_plan->{phases} }) {
      push @refusals, "item $_->{id} was removed; ids are permanent" for grep { !$new_ids{ $_->{id} } } @{ $phase->{items} };
    }
    for my $id (sort keys %{ $old_state->{status} }) {
      push @refusals, "the status of $id was dropped; change its value instead" unless exists $state->{status}{$id};
    }

    # A viewer's browser keeps its own copy and prefers whichever is newer.
    my ($old, $new) = ($old_state->{updated} // '', $state->{updated} // '');
    push @refusals, "\"updated\" must be later than the page's $old (set it to the current time)" unless $new gt $old;
  }
  if (@refusals) {
    print STDERR "REFUSED, nothing written:\n";
    print STDERR "  - $_\n" for @refusals;
    exit 1;
  }

  my $json = JSON::PP->new->utf8->canonical;
  my %body = (plan => $json->encode($plan), state => $json->encode($state));
  s/</\\u003c/g for values %body;

  # Replace the later block first so the earlier one's offsets still hold.
  for my $s (sort { $b->{body_start} <=> $a->{body_start} } $plan_script, $state_script) {
    substr($html, $s->{body_start}, $s->{body_end} - $s->{body_start}) = $body{ $s->{id} };
  }

  my ($after, $after_summary) = check_page($html);
  if (@$after) {
    print STDERR "REFUSED, the edited page would not be clean:\n";
    print STDERR "  - $_\n" for @$after;
    exit 1;
  }
  spit($out, $html);
  print "wrote $out: $after_summary\n";
}
